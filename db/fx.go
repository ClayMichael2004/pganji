package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/metrics"
	"pganji/outbox"
)

const (
	FXBridgeKESAccountID = "00000000-0000-0000-0000-000000000002"
	FXBridgeUSDAccountID = "00000000-0000-0000-0000-000000000003"
)

var fxBridgeByCurrency = map[string]string{
	"KES": FXBridgeKESAccountID,
	"USD": FXBridgeUSDAccountID,
}

// Institutional mid-market reference rates (1 unit of source = rate units of target)
var referenceFXRates = map[string]string{
	"KES_USD": "0.00775194", // ~129.00 KES per 1.00 USD
	"USD_KES": "129.00000000",
}

type FXRate struct {
	Pair          string    `json:"pair"`
	BaseCurrency  string    `json:"base_currency"`
	QuoteCurrency string    `json:"quote_currency"`
	MidRate       string    `json:"mid_rate"`
	BidRate       string    `json:"bid_rate"` // Rate bank pays customer to buy base currency
	AskRate       string    `json:"ask_rate"` // Rate bank charges customer to sell base currency
	SpreadBps     int       `json:"spread_bps"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type FXQuote struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	SourceCurrency string    `json:"source_currency"`
	TargetCurrency string    `json:"target_currency"`
	SourceAmount   int64     `json:"source_amount"`
	TargetAmount   int64     `json:"target_amount"`
	ExchangeRate   string    `json:"exchange_rate"`
	RateType       string    `json:"rate_type"` // "BID", "ASK", or "MID"
	Status         string    `json:"status"`    // "ACTIVE", "CONSUMED", "EXPIRED"
	ExpiresAt      time.Time `json:"expires_at"`
	TTLSeconds     int64     `json:"ttl_seconds"`
}

type WalletAccount struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	AccountNumber string `json:"account_number"`
	Currency      string `json:"currency"`
	Type          string `json:"type"`
	Balance       int64  `json:"balance"`
}

type FXConvertParams struct {
	UserID        string
	FromAccountID string
	ToAccountID   string
	SourceAmount  int64  // Minor units (cents) in source currency
	Reference     string
	Description   string
	QuoteID       string // Optional guaranteed RFQ quote identifier
}

type FXConvertResult struct {
	TransactionID  string `json:"transaction_id"`
	Reference      string `json:"reference"`
	FromAccountID  string `json:"from_account_id"`
	ToAccountID    string `json:"to_account_id"`
	SourceCurrency string `json:"source_currency"`
	TargetCurrency string `json:"target_currency"`
	SourceAmount   int64  `json:"source_amount"`
	TargetAmount   int64  `json:"target_amount"`
	ExchangeRate   string `json:"exchange_rate"`
	QuoteID        string `json:"quote_id,omitempty"`
	Status         string `json:"status"`
}

// ListFXRates returns the real-time buy and sell currency rates.
func ListFXRates(ctx context.Context, pool *pgxpool.Pool) ([]FXRate, error) {
	rows, err := pool.Query(ctx, `
		SELECT pair, base_currency, quote_currency, mid_rate::text, bid_rate::text, ask_rate::text, spread_bps, updated_at
		FROM fx_rates
		ORDER BY pair ASC;
	`)
	if err != nil {
		// Fallback to static memory rates if table is not yet migrated
		rates := []FXRate{
			{Pair: "USD_KES", BaseCurrency: "USD", QuoteCurrency: "KES", MidRate: "129.00000000", BidRate: "127.71000000", AskRate: "130.29000000", SpreadBps: 100, UpdatedAt: time.Now().UTC()},
			{Pair: "KES_USD", BaseCurrency: "KES", QuoteCurrency: "USD", MidRate: "0.00775194", BidRate: "0.00767442", AskRate: "0.00782946", SpreadBps: 100, UpdatedAt: time.Now().UTC()},
		}
		return rates, nil
	}
	defer rows.Close()

	var rates []FXRate
	for rows.Next() {
		var r FXRate
		if err := rows.Scan(&r.Pair, &r.BaseCurrency, &r.QuoteCurrency, &r.MidRate, &r.BidRate, &r.AskRate, &r.SpreadBps, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan fx rate: %w", err)
		}
		rates = append(rates, r)
	}
	return rates, rows.Err()
}

// GetEffectiveFXRate resolves the base mid-market exchange rate string for converting from source to target currency.
func GetEffectiveFXRate(ctx context.Context, pool *pgxpool.Pool, fromCurr, toCurr string) (string, string, error) {
	pairKey := fromCurr + "_" + toCurr
	var midRate string

	err := pool.QueryRow(ctx, `
		SELECT mid_rate::text
		FROM fx_rates
		WHERE pair = $1;
	`, pairKey).Scan(&midRate)

	if err == nil && midRate != "" {
		return midRate, "MID", nil
	}

	// Fallback to referenceFXRates
	if refRate, exists := referenceFXRates[pairKey]; exists {
		return refRate, "MID", nil
	}

	return "", "", fmt.Errorf("unsupported FX currency pair %s -> %s", fromCurr, toCurr)
}

// UpdateFXRate updates or inserts live market rates and automatically computes bid/ask spreads.
func UpdateFXRate(ctx context.Context, pool *pgxpool.Pool, pair, midRateStr string, spreadBps int) (*FXRate, error) {
	midRat, ok := new(big.Rat).SetString(midRateStr)
	if !ok || midRat.Sign() <= 0 {
		return nil, fmt.Errorf("invalid mid_rate %q: must be a positive number", midRateStr)
	}
	if spreadBps < 0 {
		return nil, fmt.Errorf("spread_bps must be non-negative")
	}

	spreadFraction := new(big.Rat).SetFrac(big.NewInt(int64(spreadBps)), big.NewInt(10000))
	one := big.NewRat(1, 1)

	// Bid = Mid * (1 - Spread)
	oneMinusSpread := new(big.Rat).Sub(one, spreadFraction)
	bidRat := new(big.Rat).Mul(midRat, oneMinusSpread)

	// Ask = Mid * (1 + Spread)
	onePlusSpread := new(big.Rat).Add(one, spreadFraction)
	askRat := new(big.Rat).Mul(midRat, onePlusSpread)

	parts := strings.Split(pair, "_")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid pair format %q: expected BASE_QUOTE (e.g. USD_KES)", pair)
	}
	baseCurr := strings.ToUpper(parts[0])
	quoteCurr := strings.ToUpper(parts[1])

	bidStr := bidRat.FloatString(8)
	askStr := askRat.FloatString(8)
	midStr := midRat.FloatString(8)

	var updated FXRate
	err := pool.QueryRow(ctx, `
		INSERT INTO fx_rates (pair, base_currency, quote_currency, mid_rate, bid_rate, ask_rate, spread_bps, updated_at)
		VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric, $7, CURRENT_TIMESTAMP)
		ON CONFLICT (pair) DO UPDATE
		SET mid_rate = EXCLUDED.mid_rate,
		    bid_rate = EXCLUDED.bid_rate,
		    ask_rate = EXCLUDED.ask_rate,
		    spread_bps = EXCLUDED.spread_bps,
		    updated_at = CURRENT_TIMESTAMP
		RETURNING pair, base_currency, quote_currency, mid_rate::text, bid_rate::text, ask_rate::text, spread_bps, updated_at;
	`, pair, baseCurr, quoteCurr, midStr, bidStr, askStr, spreadBps).Scan(
		&updated.Pair, &updated.BaseCurrency, &updated.QuoteCurrency,
		&updated.MidRate, &updated.BidRate, &updated.AskRate,
		&updated.SpreadBps, &updated.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to persist fx rate: %w", err)
	}

	return &updated, nil
}

// CreateFXQuote creates a guaranteed RFQ quote with a 30-second TTL applying the live bid/ask spread.
func CreateFXQuote(ctx context.Context, pool *pgxpool.Pool, userID, fromCurr, toCurr string, sourceAmount int64) (*FXQuote, error) {
	fromCurr = strings.ToUpper(strings.TrimSpace(fromCurr))
	toCurr = strings.ToUpper(strings.TrimSpace(toCurr))
	if fromCurr == toCurr {
		return nil, fmt.Errorf("source and target currencies must be different")
	}
	if sourceAmount <= 0 {
		return nil, fmt.Errorf("source_amount must be greater than zero")
	}

	pairKey := fromCurr + "_" + toCurr
	var (
		rateStr  string
		rateType string
		bidRate  string
	)

	err := pool.QueryRow(ctx, `
		SELECT bid_rate::text
		FROM fx_rates
		WHERE pair = $1;
	`, pairKey).Scan(&bidRate)

	if err == nil && bidRate != "" {
		rateStr = bidRate
		rateType = "BID"
	} else {
		liveRate, sType, err := GetEffectiveFXRate(ctx, pool, fromCurr, toCurr)
		if err != nil {
			return nil, err
		}
		rateStr = liveRate
		rateType = sType
	}

	targetAmount, err := computeConvertedMinorUnits(sourceAmount, rateStr)
	if err != nil {
		return nil, err
	}
	if targetAmount <= 0 {
		return nil, fmt.Errorf("source_amount too small to convert into at least 1 minor unit of %s", toCurr)
	}

	ttl := 30 * time.Second
	expiresAt := time.Now().UTC().Add(ttl)

	var quote FXQuote
	err = pool.QueryRow(ctx, `
		INSERT INTO fx_quotes (user_id, source_currency, target_currency, source_amount, target_amount, exchange_rate, rate_type, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, 'ACTIVE', $8)
		RETURNING id::text, user_id::text, source_currency, target_currency, source_amount, target_amount, exchange_rate::text, rate_type, status, expires_at;
	`, userID, fromCurr, toCurr, sourceAmount, targetAmount, rateStr, rateType, expiresAt).Scan(
		&quote.ID, &quote.UserID, &quote.SourceCurrency, &quote.TargetCurrency,
		&quote.SourceAmount, &quote.TargetAmount, &quote.ExchangeRate,
		&quote.RateType, &quote.Status, &quote.ExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to record fx quote: %w", err)
	}
	quote.TTLSeconds = int64(ttl.Seconds())

	return &quote, nil
}

// CreateWalletAccount provisions a new currency wallet (e.g., USD or KES) for an authenticated user.
func CreateWalletAccount(ctx context.Context, pool *pgxpool.Pool, userID, currency string) (*WalletAccount, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if _, supported := fxBridgeByCurrency[currency]; !supported {
		return nil, fmt.Errorf("unsupported currency %q: supported currencies are KES, USD", currency)
	}

	var suffixBytes [4]byte
	if _, err := rand.Read(suffixBytes[:]); err != nil {
		return nil, fmt.Errorf("failed to generate account suffix: %w", err)
	}
	accNumber := fmt.Sprintf("ACC-%s-%s", currency, strings.ToUpper(hex.EncodeToString(suffixBytes[:])))

	var acc WalletAccount
	err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_number, currency, type)
		VALUES ($1, $2, $3, 'ASSET')
		RETURNING id, user_id::text, account_number, currency, type;
	`, userID, accNumber, currency).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.Currency, &acc.Type)
	if err != nil {
		return nil, fmt.Errorf("failed to create wallet account: %w", err)
	}

	acc.Balance = 0
	return &acc, nil
}

// ListUserAccounts returns all currency wallets owned by the user along with live balances.
func ListUserAccounts(ctx context.Context, pool *pgxpool.Pool, userID string) ([]WalletAccount, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.id, a.user_id::text, a.account_number, a.currency, a.type,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0) AS balance
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		WHERE a.user_id::text = $1
		GROUP BY a.id, a.user_id, a.account_number, a.currency, a.type
		ORDER BY a.currency ASC, a.account_number ASC;
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query user accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]WalletAccount, 0)
	for rows.Next() {
		var a WalletAccount
		if err := rows.Scan(&a.ID, &a.UserID, &a.AccountNumber, &a.Currency, &a.Type, &a.Balance); err != nil {
			return nil, fmt.Errorf("failed to scan user account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// ConvertCurrencyTx executes an atomic cross-currency FX conversion across two currency-specific FX bridge pools.
func ConvertCurrencyTx(ctx context.Context, pool *pgxpool.Pool, params FXConvertParams) (*FXConvertResult, error) {
	if params.FromAccountID == params.ToAccountID {
		return nil, fmt.Errorf("source and target accounts must be different")
	}
	if params.SourceAmount <= 0 {
		return nil, fmt.Errorf("source_amount must be greater than zero")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin FX transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Inspect account metadata first to determine currencies and FX bridge IDs
	var fromOwner, fromCurrency string
	err = tx.QueryRow(ctx, `SELECT COALESCE(user_id::text, ''), currency FROM accounts WHERE id = $1`, params.FromAccountID).Scan(&fromOwner, &fromCurrency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("source account not found")
		}
		return nil, fmt.Errorf("failed to query source account: %w", err)
	}

	var toOwner, toCurrency string
	err = tx.QueryRow(ctx, `SELECT COALESCE(user_id::text, ''), currency FROM accounts WHERE id = $1`, params.ToAccountID).Scan(&toOwner, &toCurrency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("target account not found")
		}
		return nil, fmt.Errorf("failed to query target account: %w", err)
	}

	if fromOwner != params.UserID || toOwner != params.UserID {
		return nil, fmt.Errorf("access denied: both source and target wallets must belong to the authenticated user")
	}

	fromCurrency = strings.ToUpper(fromCurrency)
	toCurrency = strings.ToUpper(toCurrency)
	if fromCurrency == toCurrency {
		return nil, fmt.Errorf("FX conversion requires distinct currencies (both accounts are %s)", fromCurrency)
	}

	srcBridgeID := fxBridgeByCurrency[fromCurrency]
	dstBridgeID := fxBridgeByCurrency[toCurrency]

	// 2. Lock all 4 accounts (2 user wallets + 2 FX bridge accounts) in deterministic UUID order
	lockIDs := []string{params.FromAccountID, params.ToAccountID, srcBridgeID, dstBridgeID}
	sort.Strings(lockIDs)
	for _, id := range lockIDs {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id = $1 FOR UPDATE;`, id).Scan(&locked); err != nil {
			return nil, fmt.Errorf("failed to lock account %s: %w", id, err)
		}
	}

	// 3. Resolve executable rate & target minor units (Guaranteed Quote vs Live Rate)
	var (
		rateStr      string
		targetAmount int64
	)

	if strings.TrimSpace(params.QuoteID) != "" {
		var (
			qSrcCurr, qDstCurr string
			qSrcAmt, qDstAmt   int64
			qRate              string
			qStatus            string
			qExpiresAt         time.Time
		)
		qErr := tx.QueryRow(ctx, `
			SELECT source_currency, target_currency, source_amount, target_amount, exchange_rate::text, status, expires_at
			FROM fx_quotes
			WHERE id = $1 AND user_id = $2
			FOR UPDATE;
		`, params.QuoteID, params.UserID).Scan(&qSrcCurr, &qDstCurr, &qSrcAmt, &qDstAmt, &qRate, &qStatus, &qExpiresAt)
		if qErr != nil {
			if errors.Is(qErr, pgx.ErrNoRows) {
				return nil, fmt.Errorf("quote not found or belongs to another user")
			}
			return nil, fmt.Errorf("failed to lock fx quote: %w", qErr)
		}

		if qStatus != "ACTIVE" {
			return nil, fmt.Errorf("fx quote is already %s", qStatus)
		}
		if time.Now().UTC().After(qExpiresAt) {
			_, _ = tx.Exec(ctx, `UPDATE fx_quotes SET status = 'EXPIRED' WHERE id = $1`, params.QuoteID)
			return nil, fmt.Errorf("fx quote expired at %s", qExpiresAt.Format(time.RFC3339))
		}
		if qSrcCurr != fromCurrency || qDstCurr != toCurrency {
			return nil, fmt.Errorf("fx quote currency pair mismatch (%s->%s != %s->%s)", qSrcCurr, qDstCurr, fromCurrency, toCurrency)
		}
		if qSrcAmt != params.SourceAmount {
			return nil, fmt.Errorf("fx quote source_amount mismatch (quote=%d, requested=%d)", qSrcAmt, params.SourceAmount)
		}

		rateStr = qRate
		targetAmount = qDstAmt

		// Mark quote as consumed
		if _, err := tx.Exec(ctx, `UPDATE fx_quotes SET status = 'CONSUMED' WHERE id = $1;`, params.QuoteID); err != nil {
			return nil, fmt.Errorf("failed to consume quote: %w", err)
		}
	} else {
		liveRate, _, err := GetEffectiveFXRate(ctx, pool, fromCurrency, toCurrency)
		if err != nil {
			return nil, err
		}
		rateStr = liveRate

		computed, err := computeConvertedMinorUnits(params.SourceAmount, rateStr)
		if err != nil {
			return nil, err
		}
		if computed <= 0 {
			return nil, fmt.Errorf("source_amount too small to convert into at least 1 minor unit of %s", toCurrency)
		}
		targetAmount = computed
	}

	// 4. Verify sufficient funds in source wallet
	srcBalance, err := GetAccountBalance(ctx, tx, params.FromAccountID)
	if err != nil {
		return nil, err
	}
	if srcBalance < params.SourceAmount {
		return nil, fmt.Errorf("insufficient %s funds: available %d, required %d", fromCurrency, srcBalance, params.SourceAmount)
	}

	// 5. Create Transaction Header
	var txID string
	desc := params.Description
	if strings.TrimSpace(desc) == "" {
		desc = fmt.Sprintf("FX Conversion %s -> %s @ %s", fromCurrency, toCurrency, rateStr)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO transactions (reference, description, status)
		VALUES ($1, $2, 'COMPLETED')
		RETURNING id;
	`, params.Reference, desc).Scan(&txID)
	if err != nil {
		return nil, fmt.Errorf("failed to create FX transaction record: %w", err)
	}

	// 6. Record FX Conversion Audit Entry
	_, err = tx.Exec(ctx, `
		INSERT INTO fx_conversions (
			transaction_id, from_account_id, to_account_id, source_currency, target_currency, source_amount, target_amount, exchange_rate
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric);
	`, txID, params.FromAccountID, params.ToAccountID, fromCurrency, toCurrency, params.SourceAmount, targetAmount, rateStr)
	if err != nil {
		return nil, fmt.Errorf("failed to insert fx_conversions metadata: %w", err)
	}

	// 7. Post 4 Balanced Double-Entry Journal Legs (2 per currency book)
	insertEntry := `
		INSERT INTO journal_entries (transaction_id, account_id, entry_type, amount)
		VALUES ($1, $2, $3, $4);
	`
	// Source currency book (e.g., KES): User CREDIT, FX-BRIDGE-KES DEBIT
	if _, err := tx.Exec(ctx, insertEntry, txID, params.FromAccountID, "CREDIT", params.SourceAmount); err != nil {
		return nil, fmt.Errorf("failed to credit source wallet: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, txID, srcBridgeID, "DEBIT", params.SourceAmount); err != nil {
		return nil, fmt.Errorf("failed to debit source FX bridge: %w", err)
	}

	// Target currency book (e.g., USD): FX-BRIDGE-USD CREDIT, User DEBIT
	if _, err := tx.Exec(ctx, insertEntry, txID, dstBridgeID, "CREDIT", targetAmount); err != nil {
		return nil, fmt.Errorf("failed to credit target FX bridge: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, txID, params.ToAccountID, "DEBIT", targetAmount); err != nil {
		return nil, fmt.Errorf("failed to debit target wallet: %w", err)
	}

	// 8. Emit Outbox Event atomically
	payload := map[string]any{
		"transaction_id":  txID,
		"reference":       params.Reference,
		"from_account_id": params.FromAccountID,
		"to_account_id":   params.ToAccountID,
		"source_currency": fromCurrency,
		"target_currency": toCurrency,
		"source_amount":   params.SourceAmount,
		"target_amount":   targetAmount,
		"exchange_rate":   rateStr,
		"status":          "COMPLETED",
	}
	if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", txID, "fx.converted", payload); err != nil {
		return nil, fmt.Errorf("failed to insert fx.converted outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit FX conversion transaction: %w", err)
	}

	// Record Prometheus FX telemetry after commit
	metrics.FXConversionsTotal.WithLabelValues(fromCurrency, toCurrency).Inc()
	metrics.FXVolumeMinorUnitsTotal.WithLabelValues("source", fromCurrency).Add(float64(params.SourceAmount))
	metrics.FXVolumeMinorUnitsTotal.WithLabelValues("target", toCurrency).Add(float64(targetAmount))

	return &FXConvertResult{
		TransactionID:  txID,
		Reference:      params.Reference,
		FromAccountID:  params.FromAccountID,
		ToAccountID:    params.ToAccountID,
		SourceCurrency: fromCurrency,
		TargetCurrency: toCurrency,
		SourceAmount:   params.SourceAmount,
		TargetAmount:   targetAmount,
		ExchangeRate:   rateStr,
		QuoteID:        params.QuoteID,
		Status:         "COMPLETED",
	}, nil
}

func computeConvertedMinorUnits(sourceMinor int64, rateStr string) (int64, error) {
	rate, ok := new(big.Rat).SetString(rateStr)
	if !ok {
		return 0, fmt.Errorf("invalid exchange rate %q", rateStr)
	}
	amt := big.NewRat(sourceMinor, 1)
	prod := new(big.Rat).Mul(amt, rate)

	// Round rational quotient to nearest integer (half-up)
	num := new(big.Int).Set(prod.Num())
	den := new(big.Int).Set(prod.Denom())
	half := new(big.Int).Rsh(den, 1)
	num.Add(num, half)
	quotient := new(big.Int).Quo(num, den)

	return quotient.Int64(), nil
}
