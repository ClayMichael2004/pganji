package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TransferParams holds the input required to execute an atomic money transfer.
type TransferParams struct {
	FromAccountID string
	ToAccountID   string
	Amount        int64  // Cents (BIGINT in Postgres)
	Reference     string // Unique business tracking reference
	Description   string
}

// GetAccountBalance calculates live balance directly from journal entries.
func GetAccountBalance(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	query := `
		SELECT 
			COALESCE(SUM(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE 0 END), 0) -
			COALESCE(SUM(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE 0 END), 0) AS balance
		FROM journal_entries
		WHERE account_id = $1;
	`

	var balance int64
	err := tx.QueryRow(ctx, query, accountID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("failed to calculate balance: %w", err)
	}

	return balance, nil
}

// TransferTx executes a double-entry transfer atomically within a database transaction.
func TransferTx(ctx context.Context, pool *pgxpool.Pool, params TransferParams) error {
	// 1. Begin Database Transaction
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 2. Lock accounts in deterministic order to prevent deadlocks
	firstID, secondID := params.FromAccountID, params.ToAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	lockQuery := `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE;`
	rows, err := tx.Query(ctx, lockQuery, firstID, secondID)
	if err != nil {
		return fmt.Errorf("failed to acquire row locks: %w", err)
	}
	rows.Close()

	// 3. Verify sender balance
	senderBalance, err := GetAccountBalance(ctx, tx, params.FromAccountID)
	if err != nil {
		return err
	}

	if senderBalance < params.Amount {
		return fmt.Errorf("insufficient funds: available %d, required %d", senderBalance, params.Amount)
	}

	// 4. Create Transaction Header Record
	var txID string
	createTxQuery := `
		INSERT INTO transactions (reference, description, status)
		VALUES ($1, $2, 'COMPLETED')
		RETURNING id;
	`
	err = tx.QueryRow(ctx, createTxQuery, params.Reference, params.Description).Scan(&txID)
	if err != nil {
		return fmt.Errorf("failed to create transaction record: %w", err)
	}

	// 5. Insert DEBIT & CREDIT balanced entries
	insertEntryQuery := `
		INSERT INTO journal_entries (transaction_id, account_id, entry_type, amount)
		VALUES ($1, $2, $3, $4);
	`
	// Sender account gets CREDIT (money going out)
	_, err = tx.Exec(ctx, insertEntryQuery, txID, params.FromAccountID, "CREDIT", params.Amount)
	if err != nil {
		return fmt.Errorf("failed to insert sender entry: %w", err)
	}

	// Receiver account gets DEBIT (money coming in)
	_, err = tx.Exec(ctx, insertEntryQuery, txID, params.ToAccountID, "DEBIT", params.Amount)
	if err != nil {
		return fmt.Errorf("failed to insert receiver entry: %w", err)
	}

	// 6. Commit permanently
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}