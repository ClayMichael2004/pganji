package db

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/metrics"
	"pganji/outbox"
)

type ReversalParams struct {
	OriginalTransactionID string
	Reference             string
	Reason                string
	InitiatedByUserID     string
}

type ReversalResult struct {
	ReversalTransactionID string `json:"reversal_transaction_id"`
	OriginalTransactionID string `json:"original_transaction_id"`
	Reference             string `json:"reference"`
	Amount                int64  `json:"amount"`
	Status                string `json:"status"`
}

type journalLeg struct {
	AccountID string
	EntryType string
	Amount    int64
}

func isInstitutionalPoolAccount(accountID string) bool {
	return accountID == MpesaClearingAccountID ||
		accountID == FXBridgeKESAccountID ||
		accountID == FXBridgeUSDAccountID
}

// ReverseTransactionTx posts an append-only compensating double-entry transaction inverting all original legs.
func ReverseTransactionTx(ctx context.Context, pool *pgxpool.Pool, params ReversalParams) (*ReversalResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin reversal transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock the original transaction row to serialize concurrent reversal attempts
	var (
		origRef         string
		origStatus      string
		alreadyReversed *string
	)
	err = tx.QueryRow(ctx, `
		SELECT reference, status, reversed_transaction_id
		FROM transactions
		WHERE id = $1
		FOR UPDATE;
	`, params.OriginalTransactionID).Scan(&origRef, &origStatus, &alreadyReversed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("original transaction not found")
		}
		return nil, fmt.Errorf("failed to lock original transaction: %w", err)
	}

	if alreadyReversed != nil {
		return nil, fmt.Errorf("cannot reverse a reversal transaction")
	}
	if origStatus != "COMPLETED" {
		return nil, fmt.Errorf("only COMPLETED transactions can be reversed (status=%s)", origStatus)
	}

	// 2. Check if another transaction has already reversed this original transaction
	var existingReversalID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM transactions WHERE reversed_transaction_id = $1 LIMIT 1;
	`, params.OriginalTransactionID).Scan(&existingReversalID)
	if err == nil {
		return nil, fmt.Errorf("transaction already reversed by %s", existingReversalID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("failed to check existing reversals: %w", err)
	}

	// 3. Load original journal entries
	rows, err := tx.Query(ctx, `
		SELECT account_id, entry_type, amount
		FROM journal_entries
		WHERE transaction_id = $1
		ORDER BY seq ASC;
	`, params.OriginalTransactionID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch original journal entries: %w", err)
	}

	var legs []journalLeg
	accountSet := make(map[string]struct{})
	for rows.Next() {
		var l journalLeg
		if err := rows.Scan(&l.AccountID, &l.EntryType, &l.Amount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan journal leg: %w", err)
		}
		legs = append(legs, l)
		accountSet[l.AccountID] = struct{}{}
	}
	rows.Close()

	if len(legs) == 0 {
		return nil, fmt.Errorf("original transaction has no journal entries")
	}

	// 4. Lock all involved accounts in deterministic ascending UUID order to avoid deadlocks
	accountIDs := make([]string, 0, len(accountSet))
	for id := range accountSet {
		accountIDs = append(accountIDs, id)
	}
	sort.Strings(accountIDs)

	for _, accID := range accountIDs {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id = $1 FOR UPDATE;`, accID).Scan(&lockedID); err != nil {
			return nil, fmt.Errorf("failed to lock account %s: %w", accID, err)
		}
	}

	// 5. Verify funds on any user wallet account being credited (clawed back) by the reversal
	var totalReversedAmount int64
	for _, leg := range legs {
		if leg.EntryType == "DEBIT" {
			totalReversedAmount += leg.Amount
			if isInstitutionalPoolAccount(leg.AccountID) {
				continue
			}
			bal, err := GetAccountBalance(ctx, tx, leg.AccountID)
			if err != nil {
				return nil, err
			}
			if bal < leg.Amount {
				return nil, fmt.Errorf("insufficient funds for reversal clawback on account %s: available %d, required %d", leg.AccountID, bal, leg.Amount)
			}
		}
	}

	// 6. Create the Compensating Reversal Transaction Header
	var reversalTxID string
	desc := fmt.Sprintf("Reversal of %s: %s", origRef, params.Reason)
	err = tx.QueryRow(ctx, `
		INSERT INTO transactions (reference, description, status, reversed_transaction_id, reversal_reason)
		VALUES ($1, $2, 'COMPLETED', $3, $4)
		RETURNING id;
	`, params.Reference, desc, params.OriginalTransactionID, params.Reason).Scan(&reversalTxID)
	if err != nil {
		return nil, fmt.Errorf("failed to create reversal transaction record: %w", err)
	}

	// 7. Insert Inverted Journal Entries (Trigger automatically extends SHA-256 hash chain)
	insertEntry := `
		INSERT INTO journal_entries (transaction_id, account_id, entry_type, amount)
		VALUES ($1, $2, $3, $4);
	`
	for _, leg := range legs {
		invertedType := "CREDIT"
		if leg.EntryType == "CREDIT" {
			invertedType = "DEBIT"
		}
		if _, err := tx.Exec(ctx, insertEntry, reversalTxID, leg.AccountID, invertedType, leg.Amount); err != nil {
			return nil, fmt.Errorf("failed to insert inverted journal entry: %w", err)
		}
	}

	// 8. Emit Outbox Event atomically inside the same ACID transaction
	payload := map[string]any{
		"reversal_transaction_id": reversalTxID,
		"original_transaction_id": params.OriginalTransactionID,
		"original_reference":      origRef,
		"reversal_reference":      params.Reference,
		"reason":                  params.Reason,
		"amount":                  totalReversedAmount,
		"initiated_by_user_id":    params.InitiatedByUserID,
		"status":                  "REVERSED",
	}
	if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", reversalTxID, "transaction.reversed", payload); err != nil {
		return nil, fmt.Errorf("failed to record reversal outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit reversal transaction: %w", err)
	}

	revSource := "MANUAL_API"
	if params.InitiatedByUserID == "SYSTEM_CLEARING_GATEWAY" {
		revSource = "B2C_AUTO_REVERSAL"
	}
	metrics.ReversalsTotal.WithLabelValues(revSource).Inc()

	return &ReversalResult{
		ReversalTransactionID: reversalTxID,
		OriginalTransactionID: params.OriginalTransactionID,
		Reference:             params.Reference,
		Amount:                totalReversedAmount,
		Status:                "REVERSED",
	}, nil
}
