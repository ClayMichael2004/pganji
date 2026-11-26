package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestGetAccountBalance(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	// Create user
	var userID string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, full_name)
		VALUES ($1, 'hash', 'Balance Test User')
		RETURNING id::text;
	`, fmt.Sprintf("bal-user-%s@pganji.io", tag)).Scan(&userID)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	// Create account
	acc, err := CreateWalletAccount(ctx, pool, userID, "KES")
	if err != nil {
		t.Fatalf("failed to create wallet account: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	bal, err := GetAccountBalance(ctx, tx, acc.ID)
	if err != nil {
		t.Fatalf("failed to get balance: %v", err)
	}
	if bal != 0 {
		t.Errorf("expected initial balance 0, got %d", bal)
	}
}

func TestTransferTxInsufficientFunds(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	var userID string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, full_name)
		VALUES ($1, 'hash', 'Transfer Test User')
		RETURNING id::text;
	`, fmt.Sprintf("xfer-user-%s@pganji.io", tag)).Scan(&userID)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	acc1, err := CreateWalletAccount(ctx, pool, userID, "KES")
	if err != nil {
		t.Fatalf("failed to create acc1: %v", err)
	}
	acc2, err := CreateWalletAccount(ctx, pool, userID, "KES")
	if err != nil {
		t.Fatalf("failed to create acc2: %v", err)
	}

	// Attempt transfer without deposit -> should fail with insufficient funds
	err = TransferTx(ctx, pool, TransferParams{
		FromAccountID: acc1.ID,
		ToAccountID:   acc2.ID,
		Amount:        5000,
		Reference:     "TX-FAIL-" + tag,
		Description:   "Test insufficient funds",
	})
	if err == nil {
		t.Fatalf("expected error on insufficient funds, got nil")
	}
}
