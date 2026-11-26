package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/metrics"
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

type B2CCallbackParams struct {
	ExternalReference string // Matches OriginatorConversationID / ConversationID
	ProviderReceipt   string // Safaricom M-Pesa TransactionID (e.g. "QKL8921XYZ")
	ResultCode        int    // 0 = Success, non-zero = Failed
	ResultDesc        string
	IsTimeout         bool
}

type B2CSettlementOutcome struct {
	ExternalReference     string `json:"external_reference"`
	OriginalTransactionID string `json:"original_transaction_id"`
	Status                string `json:"status"` // "SETTLED", "FAILED", or "TIMED_OUT"
	ReversalTransactionID string `json:"reversal_transaction_id,omitempty"`
	ReversedAmount        int64  `json:"reversed_amount,omitempty"`
}

// DepositTx credits the M-Pesa clearing pool and debits the user's ledger account atomically.
func DepositTx(ctx context.Context, pool *pgxpool.Pool, params ClearingParams) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin deposit tx: %w", err)
	}
	defer tx.Rollback(ctx)

	firstID, secondID := MpesaClearingAccountID, params.UserAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	rows, err := tx.Query(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) ORDER BY id ASC FOR UPDATE;`, firstID, secondID)
	if err != nil {
		return "", fmt.Errorf("failed to lock accounts: %w", err)
	}
	rows.Close()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, params.UserAccountID).Scan(&exists); err != nil || !exists {
		return "", fmt.Errorf("target user account not found")
	}

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

	_, err = tx.Exec(ctx, `
		INSERT INTO external_clearing_records (
			transaction_id, provider, external_reference, phone_number, direction, amount, status, provider_receipt, result_code, result_desc, settled_at
		)
		VALUES ($1, $2, $3, $4, 'DEPOSIT', $5, 'SETTLED', $3, 0, 'Confirmed C2B Deposit', CURRENT_TIMESTAMP);
	`, txID, params.Provider, params.ExternalReference, params.PhoneNumber, params.Amount)
	if err != nil {
		return "", fmt.Errorf("failed to record external clearing metadata: %w", err)
	}

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

	metrics.ClearingOperationsTotal.WithLabelValues(params.Provider, "DEPOSIT", "SETTLED").Inc()
	metrics.ClearingVolumeMinorUnitsTotal.WithLabelValues("DEPOSIT", "SETTLED").Add(float64(params.Amount))

	return txID, nil
}

// WithdrawTx validates user funds, debits the clearing pool, credits the user account, and emits a B2C payout event.
func WithdrawTx(ctx context.Context, pool *pgxpool.Pool, params ClearingParams) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin withdrawal tx: %w", err)
	}
	defer tx.Rollback(ctx)

	firstID, secondID := MpesaClearingAccountID, params.UserAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	rows, err := tx.Query(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) ORDER BY id ASC FOR UPDATE;`, firstID, secondID)
	if err != nil {
		return "", fmt.Errorf("failed to lock accounts: %w", err)
	}
	rows.Close()

	balance, err := GetAccountBalance(ctx, tx, params.UserAccountID)
	if err != nil {
		return "", err
	}
	if balance < params.Amount {
		return "", fmt.Errorf("insufficient funds: available %d, required %d", balance, params.Amount)
	}

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

	_, err = tx.Exec(ctx, `
		INSERT INTO external_clearing_records (
			transaction_id, provider, external_reference, phone_number, direction, amount, status
		)
		VALUES ($1, $2, $3, $4, 'WITHDRAWAL', $5, 'PENDING');
	`, txID, params.Provider, params.ExternalReference, params.PhoneNumber, params.Amount)
	if err != nil {
		return "", fmt.Errorf("failed to record withdrawal clearing metadata: %w", err)
	}

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

	metrics.ClearingOperationsTotal.WithLabelValues(params.Provider, "WITHDRAWAL", "INITIATED").Inc()
	metrics.ClearingVolumeMinorUnitsTotal.WithLabelValues("WITHDRAWAL", "INITIATED").Add(float64(params.Amount))

	return txID, nil
}

// SettleB2CResultTx finalizes a pending B2C withdrawal on ResultCode=0 or auto-reverses funds on failure/timeout.
func SettleB2CResultTx(ctx context.Context, pool *pgxpool.Pool, params B2CCallbackParams) (*B2CSettlementOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin B2C settlement tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		recordID      string
		origTxID      string
		direction     string
		currentStatus string
		phoneNumber   string
		amount        int64
	)

	err = tx.QueryRow(ctx, `
		SELECT id, transaction_id, direction, status, phone_number, amount
		FROM external_clearing_records
		WHERE external_reference = $1
		FOR UPDATE;
	`, params.ExternalReference).Scan(&recordID, &origTxID, &direction, &currentStatus, &phoneNumber, &amount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("clearing record not found for external_reference %s", params.ExternalReference)
		}
		return nil, fmt.Errorf("failed to lock clearing record: %w", err)
	}

	if direction != "WITHDRAWAL" {
		return nil, fmt.Errorf("clearing record %s is not a B2C withdrawal", params.ExternalReference)
	}

	if currentStatus != "PENDING" {
		return nil, fmt.Errorf("B2C payout %s already finalized with status %s", params.ExternalReference, currentStatus)
	}

	// Case 1: Safaricom confirmed payout delivery (ResultCode == 0 && !IsTimeout)
	if !params.IsTimeout && params.ResultCode == 0 {
		_, err = tx.Exec(ctx, `
			UPDATE external_clearing_records
			SET status = 'SETTLED',
			    provider_receipt = NULLIF($2, ''),
			    result_code = $3,
			    result_desc = $4,
			    settled_at = CURRENT_TIMESTAMP
			WHERE id = $1;
		`, recordID, params.ProviderReceipt, params.ResultCode, params.ResultDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to mark B2C clearing record settled: %w", err)
		}

		payload := map[string]any{
			"transaction_id":     origTxID,
			"external_reference": params.ExternalReference,
			"provider_receipt":   params.ProviderReceipt,
			"phone_number":       phoneNumber,
			"amount":             amount,
			"status":             "SETTLED",
		}
		if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", origTxID, "withdrawal.settled", payload); err != nil {
			return nil, fmt.Errorf("failed to insert withdrawal.settled outbox event: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("failed to commit B2C settlement: %w", err)
		}

		metrics.ClearingOperationsTotal.WithLabelValues("MPESA", "WITHDRAWAL", "SETTLED").Inc()
		metrics.ClearingVolumeMinorUnitsTotal.WithLabelValues("WITHDRAWAL", "SETTLED").Add(float64(amount))

		return &B2CSettlementOutcome{
			ExternalReference:     params.ExternalReference,
			OriginalTransactionID: origTxID,
			Status:                "SETTLED",
		}, nil
	}

	// Case 2: Payout failed or timed out -> mark terminal state, emit withdrawal.failed, and run compensating reversal
	finalStatus := "FAILED"
	reasonPrefix := "M-Pesa B2C payout failed"
	if params.IsTimeout {
		finalStatus = "TIMED_OUT"
		reasonPrefix = "M-Pesa B2C queue timeout"
	}

	_, err = tx.Exec(ctx, `
		UPDATE external_clearing_records
		SET status = $2,
		    result_code = $3,
		    result_desc = $4,
		    settled_at = CURRENT_TIMESTAMP
		WHERE id = $1;
	`, recordID, finalStatus, params.ResultCode, params.ResultDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to update failed B2C clearing status: %w", err)
	}

	failPayload := map[string]any{
		"transaction_id":     origTxID,
		"external_reference": params.ExternalReference,
		"result_code":        params.ResultCode,
		"result_desc":        params.ResultDesc,
		"phone_number":       phoneNumber,
		"amount":             amount,
		"status":             finalStatus,
	}
	if err := outbox.InsertEvent(ctx, tx, "TRANSACTION", origTxID, "withdrawal.failed", failPayload); err != nil {
		return nil, fmt.Errorf("failed to insert withdrawal.failed outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit B2C failure status: %w", err)
	}

	metrics.ClearingOperationsTotal.WithLabelValues("MPESA", "WITHDRAWAL", finalStatus).Inc()
	metrics.ClearingVolumeMinorUnitsTotal.WithLabelValues("WITHDRAWAL", finalStatus).Add(float64(amount))

	// Execute append-only compensating reversal to refund the user's wallet
	revReason := fmt.Sprintf("%s (code=%d): %s", reasonPrefix, params.ResultCode, params.ResultDesc)
	revResult, err := ReverseTransactionTx(ctx, pool, ReversalParams{
		OriginalTransactionID: origTxID,
		Reference:             fmt.Sprintf("AUTO-REV-%s", params.ExternalReference),
		Reason:                revReason,
		InitiatedByUserID:     "SYSTEM_CLEARING_GATEWAY",
	})
	if err != nil {
		return nil, fmt.Errorf("B2C marked %s but auto-reversal failed: %w", finalStatus, err)
	}

	return &B2CSettlementOutcome{
		ExternalReference:     params.ExternalReference,
		OriginalTransactionID: origTxID,
		Status:                finalStatus,
		ReversalTransactionID: revResult.ReversalTransactionID,
		ReversedAmount:        revResult.Amount,
	}, nil
}
