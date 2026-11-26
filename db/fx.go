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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	Status         string `json:"status"`
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

	pairKey := fromCurrency + "_" + toCurrency
	rateStr, ok := referenceFXRates[pairKey]
	if !ok {
		return nil, fmt.Errorf("unsupported FX currency pair %s -> %s", fromCurrency, toCurrency)
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

	// 3. Verify sufficient funds in source wallet
	srcBalance, err := GetAccountBalance(ctx, tx, params.FromAccountID)
	if err != nil {
		return nil, err
	}
	if srcBalance < params.SourceAmount {
		return nil, fmt.Errorf("insufficient %s funds: available %d, required %d", fromCurrency, srcBalance, params.SourceAmount)
	}

	// 4. Exact rational arithmetic for minor-unit conversion (half-even rounding)
	targetAmount, err := computeConvertedMinorUnits(params.SourceAmount, rateStr)
	if err != nil {
		return nil, err
	}
	if targetAmount <= 0 {
		return nil, fmt.Errorf("source_amount too small to convert into at least 1 minor unit of %s", toCurrency)
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
