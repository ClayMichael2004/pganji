package db

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		home, _ := os.UserHomeDir()
		socketDir := filepath.Join(home, "my_postgres", "sockets")
		dbURL = fmt.Sprintf("postgres:///pganji?host=%s&port=5433", socketDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := ConnectDB(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping test: database connection failed: %v", err)
	}
	return pool
}

func TestComputeConvertedMinorUnits(t *testing.T) {
	tests := []struct {
		name        string
		sourceMinor int64
		rateStr     string
		expected    int64
		expectErr   bool
	}{
		{
			name:        "129,000 KES cents to USD at ~129.00 KES/USD",
			sourceMinor: 129000,
			rateStr:     "0.00775194",
			expected:    1000, // 10.00 USD
			expectErr:   false,
		},
		{
			name:        "1,000 USD cents to KES at 129.00 KES/USD",
			sourceMinor: 1000,
			rateStr:     "129.00000000",
			expected:    129000, // 1,290.00 KES
			expectErr:   false,
		},
		{
			name:        "Half-up rounding threshold 0.5 round up",
			sourceMinor: 100,
			rateStr:     "0.005", // 100 * 0.005 = 0.5 -> rounds to 1
			expected:    1,
			expectErr:   false,
		},
		{
			name:        "Invalid rate format",
			sourceMinor: 100,
			rateStr:     "not-a-number",
			expected:    0,
			expectErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := computeConvertedMinorUnits(tc.sourceMinor, tc.rateStr)
			if tc.expectErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.expectErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res != tc.expected {
				t.Errorf("expected %d, got %d", tc.expected, res)
			}
		})
	}
}

func TestFXBidAskSpreadCalculation(t *testing.T) {
	// Mid = 129.00, Spread = 100 bps (1.00%)
	midRat, _ := new(big.Rat).SetString("129.00000000")
	spreadBps := 100
	spreadFrac := new(big.Rat).SetFrac(big.NewInt(int64(spreadBps)), big.NewInt(10000)) // 0.01

	// Bid = Mid * (1 - 0.01) = 127.71
	one := big.NewRat(1, 1)
	bidRat := new(big.Rat).Mul(midRat, new(big.Rat).Sub(one, spreadFrac))
	expectedBid := "127.71000000"
	if bidRat.FloatString(8) != expectedBid {
		t.Errorf("expected bid %s, got %s", expectedBid, bidRat.FloatString(8))
	}

	// Ask = Mid * (1 + 0.01) = 130.29
	askRat := new(big.Rat).Mul(midRat, new(big.Rat).Add(one, spreadFrac))
	expectedAsk := "130.29000000"
	if askRat.FloatString(8) != expectedAsk {
		t.Errorf("expected ask %s, got %s", expectedAsk, askRat.FloatString(8))
	}
}

func TestDynamicFXRatesAndQuotes(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	ctx := context.Background()

	// Ensure migration 9 tables exist
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS fx_rates (
			pair VARCHAR(16) PRIMARY KEY,
			base_currency VARCHAR(3) NOT NULL,
			quote_currency VARCHAR(3) NOT NULL,
			mid_rate NUMERIC(18, 8) NOT NULL CHECK (mid_rate > 0),
			bid_rate NUMERIC(18, 8) NOT NULL CHECK (bid_rate > 0),
			ask_rate NUMERIC(18, 8) NOT NULL CHECK (ask_rate > 0),
			spread_bps INTEGER NOT NULL DEFAULT 100 CHECK (spread_bps >= 0),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS fx_quotes (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			source_currency VARCHAR(3) NOT NULL,
			target_currency VARCHAR(3) NOT NULL,
			source_amount BIGINT NOT NULL CHECK (source_amount > 0),
			target_amount BIGINT NOT NULL CHECK (target_amount > 0),
			exchange_rate NUMERIC(18, 8) NOT NULL CHECK (exchange_rate > 0),
			rate_type VARCHAR(8) NOT NULL CHECK (rate_type IN ('BID', 'ASK', 'MID')),
			status VARCHAR(16) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CONSUMED', 'EXPIRED')),
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatalf("failed to prepare test tables: %v", err)
	}

	// 1. Test UpdateFXRate
	updated, err := UpdateFXRate(ctx, pool, "USD_KES", "130.00000000", 150)
	if err != nil {
		t.Fatalf("UpdateFXRate failed: %v", err)
	}
	if updated.MidRate != "130.00000000" {
		t.Errorf("expected mid 130.00, got %s", updated.MidRate)
	}
	// 130 * (1 - 0.015) = 128.05
	if updated.BidRate != "128.05000000" {
		t.Errorf("expected bid 128.05000000, got %s", updated.BidRate)
	}
	// 130 * (1 + 0.015) = 131.95
	if updated.AskRate != "131.95000000" {
		t.Errorf("expected ask 131.95000000, got %s", updated.AskRate)
	}

	// 2. Test ListFXRates
	rates, err := ListFXRates(ctx, pool)
	if err != nil {
		t.Fatalf("ListFXRates failed: %v", err)
	}
	if len(rates) == 0 {
		t.Errorf("expected at least 1 FX rate, got 0")
	}

	// 3. Test CreateFXQuote
	var testUserID string
	err = pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, full_name)
		VALUES ($1, 'hash', 'FX Test User')
		ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name
		RETURNING id::text;
	`, fmt.Sprintf("fx-test-%d@pganji.io", time.Now().UnixNano())).Scan(&testUserID)
	if err != nil {
		t.Fatalf("failed to insert test user: %v", err)
	}

	quote, err := CreateFXQuote(ctx, pool, testUserID, "USD", "KES", 1000) // $10.00
	if err != nil {
		t.Fatalf("CreateFXQuote failed: %v", err)
	}
	if quote.ID == "" || quote.Status != "ACTIVE" {
		t.Errorf("unexpected quote state: %+v", quote)
	}
	if quote.TargetAmount <= 0 {
		t.Errorf("expected positive target amount, got %d", quote.TargetAmount)
	}
}

func TestFXQuoteExecutionAndConsumption(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	var userID string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, full_name)
		VALUES ($1, 'hash', 'Quote Exec User')
		RETURNING id::text;
	`, fmt.Sprintf("qexec-%s@pganji.io", tag)).Scan(&userID)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	kesWallet, err := CreateWalletAccount(ctx, pool, userID, "KES")
	if err != nil {
		t.Fatalf("failed to create KES wallet: %v", err)
	}
	usdWallet, err := CreateWalletAccount(ctx, pool, userID, "USD")
	if err != nil {
		t.Fatalf("failed to create USD wallet: %v", err)
	}

	// Fund KES wallet with 500,000 cents
	_, err = DepositTx(ctx, pool, ClearingParams{
		UserAccountID:     kesWallet.ID,
		Amount:            500000,
		Provider:          "MPESA_C2B",
		ExternalReference: "DEP-Q-" + tag,
		PhoneNumber:       "254712345678",
		Description:       "Deposit for Quote Test",
	})
	if err != nil {
		t.Fatalf("failed to fund KES wallet: %v", err)
	}

	// Request quote to convert 130,290 KES cents -> USD
	quote, err := CreateFXQuote(ctx, pool, userID, "KES", "USD", 130290)
	if err != nil {
		t.Fatalf("failed to create quote: %v", err)
	}
	if quote.Status != "ACTIVE" {
		t.Fatalf("expected ACTIVE quote, got %s", quote.Status)
	}

	// Convert using the quote
	res, err := ConvertCurrencyTx(ctx, pool, FXConvertParams{
		UserID:        userID,
		FromAccountID: kesWallet.ID,
		ToAccountID:   usdWallet.ID,
		SourceAmount:  130290,
		Reference:     "FX-TX-Q-" + tag,
		Description:   "Quote conversion test",
		QuoteID:       quote.ID,
	})
	if err != nil {
		t.Fatalf("ConvertCurrencyTx with quote failed: %v", err)
	}
	if res.QuoteID != quote.ID {
		t.Errorf("expected result to reference quote ID %s, got %s", quote.ID, res.QuoteID)
	}

	// Verify quote status is now CONSUMED in DB
	var quoteStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM fx_quotes WHERE id = $1`, quote.ID).Scan(&quoteStatus)
	if err != nil || quoteStatus != "CONSUMED" {
		t.Fatalf("expected quote status CONSUMED, got %s (err=%v)", quoteStatus, err)
	}

	// Attempt to reuse the consumed quote -> must fail
	_, err = ConvertCurrencyTx(ctx, pool, FXConvertParams{
		UserID:        userID,
		FromAccountID: kesWallet.ID,
		ToAccountID:   usdWallet.ID,
		SourceAmount:  130290,
		Reference:     "FX-TX-DUP-" + tag,
		Description:   "Duplicate quote reuse test",
		QuoteID:       quote.ID,
	})
	if err == nil {
		t.Fatalf("expected error when attempting to reuse consumed quote, got nil")
	}
}
