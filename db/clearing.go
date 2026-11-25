package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/outbox"
)

const MpesaClearingAccountID = "00000000-0000-0000-0000-000000000001"

type ClearingParams struct {
	UserAccountID     string
	Amount            int64
	Provider          string
	ExternalReference string
	PhoneNumber       string
	Description       string
}

// DepositTx credits the M-Pesa clearing pool and debits the user's ledger account atomically.
func DepositTx(ctx context.Context, pool *pgxpool.Pool, params ClearingParams) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin deposit tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock user account & clearing account in deterministic UUID order
	firstID, secondID := MpesaClearingAccountID, params.UserAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	rows, err := tx.Query(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE;`, firstID, secondID)
	if err != nil {
		return "", fmt.Errorf("failed to lock accounts: %w", err)
	}
	rows.Close()

	// 2. Verify target user account exists
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, params.UserAccountID).Scan(&exists); err != nil || !exists {
		return "", fmt.Errorf("target user account not found")
	}

	// 3. Create Transaction Header
	var txID string
	txRef := fmt.Sprintf("DEP-%s-%s", params.Provider, params.ExternalReference)
	err = tx.QueryRow(ctx, `
		INSERT INTO transactions (reference, description, status)
		VALUES ($1, $2, 'COMPLETED')
		RETURNING id;
	`, txRef, params.Description).Scan(&txID)
	if err != nil {
		return "", fmt.Errorf("failed to create deposit transaction: %w", err)
	}

	// 4. Insert External Clearing Audit Record (enforces unique external_reference)
	_, err = tx.Exec(ctx, `
		INSERT INTO external_clearing_records (transaction_id, provider, external_reference, phone_number, direction, amount)
		VALUES ($1, $2, $3, $4, 'DEPOSIT', $5);
	`, txID, params.Provider, params.ExternalReference, params.PhoneNumber, params.Amount)
	if err != nil {
		return "", fmt.Errorf("failed to record external clearing metadata: %w", err)
	}

	// 5. Balanced Double-Entry Journal Rows (DEBIT User Wallet, CREDIT Clearing Pool)
	insertEntry := `
		INSERT INTO journal_entries (transaction_id, account_id, entry_type, amount)
		VALUES ($1, $2, $3, $4);
	`
	if _, err := tx.Exec(ctx, insertEntry, txID, params.UserAccountID, "DEBIT", params.Amount); err != nil {
		return "", fmt.Errorf("failed to debit user account: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, txID, MpesaClearingAccountID, "CREDIT", params.Amount); err != nil {
		return "", fmt.Errorf("failed to credit clearing account: %w", err)
	}

	// 6. Emit Outbox Event atomically
	payload := map[string]any{
		"transaction_id":     txID,
		"reference":          txRef,
		"external_reference": params.ExternalReference,
		"provider":           params.Provider,
		"account_id":         params.UserAccountID,
		"phone_number":       params.PhoneNumber,
		"amount":             params.Amount,
		"direction":          "DEPOSIT",
	}
	if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", txID, "deposit.completed", payload); err != nil {
		return "", fmt.Errorf("failed to insert deposit outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("failed to commit deposit tx: %w", err)
	}

	return txID, nil
}

// WithdrawTx validates user funds, debits the clearing pool, credits the user account, and emits a B2C payout event.
func WithdrawTx(ctx context.Context, pool *pgxpool.Pool, params ClearingParams) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin withdrawal tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Deterministic Row Locking
	firstID, secondID := MpesaClearingAccountID, params.UserAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	rows, err := tx.Query(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE;`, firstID, secondID)
	if err != nil {
		return "", fmt.Errorf("failed to lock accounts: %w", err)
	}
	rows.Close()

	// 2. Enforce Available Balance Guard
	balance, err := GetAccountBalance(ctx, tx, params.UserAccountID)
	if err != nil {
		return "", err
	}
	if balance < params.Amount {
		return "", fmt.Errorf("insufficient funds: available %d, required %d", balance, params.Amount)
	}

	// 3. Create Transaction Header
	var txID string
	txRef := fmt.Sprintf("WDR-%s-%s", params.Provider, params.ExternalReference)
	err = tx.QueryRow(ctx, `
		INSERT INTO transactions (reference, description, status)
		VALUES ($1, $2, 'COMPLETED')
		RETURNING id;
	`, txRef, params.Description).Scan(&txID)
	if err != nil {
		return "", fmt.Errorf("failed to create withdrawal transaction: %w", err)
	}

	// 4. Insert External Clearing Audit Record
	_, err = tx.Exec(ctx, `
		INSERT INTO external_clearing_records (transaction_id, provider, external_reference, phone_number, direction, amount)
		VALUES ($1, $2, $3, $4, 'WITHDRAWAL', $5);
	`, txID, params.Provider, params.ExternalReference, params.PhoneNumber, params.Amount)
	if err != nil {
		return "", fmt.Errorf("failed to record withdrawal clearing metadata: %w", err)
	}

	// 5. Balanced Double-Entry Journal Rows (CREDIT User Wallet, DEBIT Clearing Pool)
	insertEntry := `
		INSERT INTO journal_entries (transaction_id, account_id, entry_type, amount)
		VALUES ($1, $2, $3, $4);
	`
	if _, err := tx.Exec(ctx, insertEntry, txID, params.UserAccountID, "CREDIT", params.Amount); err != nil {
		return "", fmt.Errorf("failed to credit user account: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, txID, MpesaClearingAccountID, "DEBIT", params.Amount); err != nil {
		return "", fmt.Errorf("failed to debit clearing account: %w", err)
	}

	// 6. Emit Outbox Event so background worker dispatches B2C payout instruction via Webhook
	payload := map[string]any{
		"transaction_id":     txID,
		"reference":          txRef,
		"external_reference": params.ExternalReference,
		"provider":           params.Provider,
		"account_id":         params.UserAccountID,
		"phone_number":       params.PhoneNumber,
		"amount":             params.Amount,
		"direction":          "WITHDRAWAL",
	}
	if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", txID, "withdrawal.initiated", payload); err != nil {
		return "", fmt.Errorf("failed to insert withdrawal outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("failed to commit withdrawal tx: %w", err)
	}

	return txID, nil
}
