package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type CurrencyTrialBalance struct {
	Currency     string `json:"currency"`
	TotalDebits  int64  `json:"total_debits"`
	TotalCredits int64  `json:"total_credits"`
	NetBalance   int64  `json:"net_balance"`
	Balanced     bool   `json:"balanced"`
}

type SystemAccountPosition struct {
	AccountID     string `json:"account_id"`
	AccountNumber string `json:"account_number"`
	Currency      string `json:"currency"`
	AccountType   string `json:"account_type"`
	Role          string `json:"role"` // "MPESA_CLEARING", "FX_BRIDGE", or "USER_WALLETS_AGGREGATE"
	TotalDebits   int64  `json:"total_debits"`
	TotalCredits  int64  `json:"total_credits"`
	NetBalance    int64  `json:"net_balance"`
}

type ClearingBucket struct {
	Direction   string `json:"direction"`
	Status      string `json:"status"`
	Count       int64  `json:"count"`
	TotalAmount int64  `json:"total_amount"`
}

type ClearingReconciliation struct {
	Buckets                     []ClearingBucket `json:"buckets"`
	SettledDepositsTotal        int64            `json:"settled_deposits_total"`
	SettledWithdrawalsTotal     int64            `json:"settled_withdrawals_total"`
	PendingWithdrawalsTotal     int64            `json:"pending_withdrawals_total"`
	ReversedWithdrawalsTotal    int64            `json:"reversed_withdrawals_total"`
	ExpectedClearingPoolBalance int64            `json:"expected_clearing_pool_balance"`
	ActualClearingPoolBalance   int64            `json:"actual_clearing_pool_balance"`
	ClearingDiscrepancy         int64            `json:"clearing_discrepancy"`
	Reconciled                  bool             `json:"reconciled"`
}

type OutboxHealthBucket struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type ReconciliationReport struct {
	GeneratedAt            time.Time               `json:"generated_at"`
	OverallStatus          string                  `json:"overall_status"` // "RECONCILED" or "DISCREPANCY_DETECTED"
	CurrencyTrialBalances  []CurrencyTrialBalance  `json:"currency_trial_balances"`
	InstitutionalPositions []SystemAccountPosition `json:"institutional_positions"`
	ClearingReconciliation ClearingReconciliation  `json:"clearing_reconciliation"`
	OutboxSummary          []OutboxHealthBucket    `json:"outbox_summary"`
}

// GenerateReconciliationReport computes per-currency trial balances, institutional bridge/clearing positions, and M-Pesa clearing reconciliation.
func GenerateReconciliationReport(ctx context.Context, pool *pgxpool.Pool) (*ReconciliationReport, error) {
	report := &ReconciliationReport{
		GeneratedAt:            time.Now().UTC(),
		OverallStatus:          "RECONCILED",
		CurrencyTrialBalances:  make([]CurrencyTrialBalance, 0),
		InstitutionalPositions: make([]SystemAccountPosition, 0),
		ClearingReconciliation: ClearingReconciliation{
			Buckets: make([]ClearingBucket, 0),
		},
		OutboxSummary: make([]OutboxHealthBucket, 0),
	}

	// 1. Per-Currency Trial Balance
	tbRows, err := pool.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE 0 END), 0) AS total_debits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'CREDIT' THEN j.amount ELSE 0 END), 0) AS total_credits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0) AS net_balance
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		GROUP BY a.currency
		ORDER BY a.currency ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query per-currency trial balance: %w", err)
	}
	defer tbRows.Close()

	for tbRows.Next() {
		var tb CurrencyTrialBalance
		if err := tbRows.Scan(&tb.Currency, &tb.TotalDebits, &tb.TotalCredits, &tb.NetBalance); err != nil {
			return nil, fmt.Errorf("failed to scan currency trial balance: %w", err)
		}
		tb.Balanced = (tb.NetBalance == 0)
		if !tb.Balanced {
			report.OverallStatus = "DISCREPANCY_DETECTED"
		}
		report.CurrencyTrialBalances = append(report.CurrencyTrialBalances, tb)
	}
	if err := tbRows.Err(); err != nil {
		return nil, err
	}

	// 2. Institutional Clearing & FX Bridge Positions (system accounts where user_id IS NULL)
	sysRows, err := pool.Query(ctx, `
		SELECT a.id::text, a.account_number, a.currency, a.type,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE 0 END), 0) AS total_debits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'CREDIT' THEN j.amount ELSE 0 END), 0) AS total_credits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0) AS net_balance
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		WHERE a.user_id IS NULL
		GROUP BY a.id, a.account_number, a.currency, a.type
		ORDER BY a.account_number ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query institutional account positions: %w", err)
	}
	defer sysRows.Close()

	var actualMpesaClearingBalance int64
	for sysRows.Next() {
		var pos SystemAccountPosition
		if err := sysRows.Scan(&pos.AccountID, &pos.AccountNumber, &pos.Currency, &pos.AccountType, &pos.TotalDebits, &pos.TotalCredits, &pos.NetBalance); err != nil {
			return nil, fmt.Errorf("failed to scan institutional position: %w", err)
		}
		switch pos.AccountID {
		case MpesaClearingAccountID:
			pos.Role = "MPESA_CLEARING"
			actualMpesaClearingBalance = pos.NetBalance
		case FXBridgeKESAccountID, FXBridgeUSDAccountID:
			pos.Role = "FX_BRIDGE"
		default:
			pos.Role = "SYSTEM_POOL"
		}
		report.InstitutionalPositions = append(report.InstitutionalPositions, pos)
	}
	if err := sysRows.Err(); err != nil {
		return nil, err
	}

	// 3. Aggregate User Wallet Positions per Currency
	usrRows, err := pool.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE 0 END), 0) AS total_debits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'CREDIT' THEN j.amount ELSE 0 END), 0) AS total_credits,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0) AS net_balance
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		WHERE a.user_id IS NOT NULL
		GROUP BY a.currency
		ORDER BY a.currency ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query aggregate user wallet positions: %w", err)
	}
	defer usrRows.Close()

	for usrRows.Next() {
		var pos SystemAccountPosition
		if err := usrRows.Scan(&pos.Currency, &pos.TotalDebits, &pos.TotalCredits, &pos.NetBalance); err != nil {
			return nil, fmt.Errorf("failed to scan aggregate user wallet position: %w", err)
		}
		pos.AccountID = "AGGREGATE"
		pos.AccountNumber = fmt.Sprintf("USER-WALLETS-%s", pos.Currency)
		pos.AccountType = "ASSET"
		pos.Role = "USER_WALLETS_AGGREGATE"
		report.InstitutionalPositions = append(report.InstitutionalPositions, pos)
	}
	if err := usrRows.Err(); err != nil {
		return nil, err
	}

	// 4. External Clearing Records Reconciliation
	clrRows, err := pool.Query(ctx, `
		SELECT direction, status, COUNT(*), COALESCE(SUM(amount), 0)
		FROM external_clearing_records
		GROUP BY direction, status
		ORDER BY direction ASC, status ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query external clearing buckets: %w", err)
	}
	defer clrRows.Close()

	for clrRows.Next() {
		var b ClearingBucket
		if err := clrRows.Scan(&b.Direction, &b.Status, &b.Count, &b.TotalAmount); err != nil {
			return nil, fmt.Errorf("failed to scan clearing bucket: %w", err)
		}
		report.ClearingReconciliation.Buckets = append(report.ClearingReconciliation.Buckets, b)

		switch {
		case b.Direction == "DEPOSIT" && b.Status == "SETTLED":
			report.ClearingReconciliation.SettledDepositsTotal += b.TotalAmount
		case b.Direction == "WITHDRAWAL" && b.Status == "SETTLED":
			report.ClearingReconciliation.SettledWithdrawalsTotal += b.TotalAmount
		case b.Direction == "WITHDRAWAL" && b.Status == "PENDING":
			report.ClearingReconciliation.PendingWithdrawalsTotal += b.TotalAmount
		case b.Direction == "WITHDRAWAL" && (b.Status == "FAILED" || b.Status == "TIMED_OUT"):
			report.ClearingReconciliation.ReversedWithdrawalsTotal += b.TotalAmount
		}
	}
	if err := clrRows.Err(); err != nil {
		return nil, err
	}

	// In CLEARING-MPESA-KES:
	// - Each C2B deposit credits CLEARING-MPESA-KES (-amount)
	// - Each active/settled B2C withdrawal debits CLEARING-MPESA-KES (+amount)
	// - Failed/Timed-out withdrawals were debited then credited back via compensating reversal (net 0)
	expectedClearing := report.ClearingReconciliation.SettledWithdrawalsTotal +
		report.ClearingReconciliation.PendingWithdrawalsTotal -
		report.ClearingReconciliation.SettledDepositsTotal

	report.ClearingReconciliation.ExpectedClearingPoolBalance = expectedClearing
	report.ClearingReconciliation.ActualClearingPoolBalance = actualMpesaClearingBalance
	report.ClearingReconciliation.ClearingDiscrepancy = actualMpesaClearingBalance - expectedClearing
	report.ClearingReconciliation.Reconciled = (report.ClearingReconciliation.ClearingDiscrepancy == 0)

	if !report.ClearingReconciliation.Reconciled {
		report.OverallStatus = "DISCREPANCY_DETECTED"
	}

	// 5. Outbox Delivery Summary
	obRows, err := pool.Query(ctx, `
		SELECT status, COUNT(*)
		FROM outbox_events
		GROUP BY status
		ORDER BY status ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query outbox summary: %w", err)
	}
	defer obRows.Close()

	for obRows.Next() {
		var ob OutboxHealthBucket
		if err := obRows.Scan(&ob.Status, &ob.Count); err != nil {
			return nil, fmt.Errorf("failed to scan outbox summary: %w", err)
		}
		report.OutboxSummary = append(report.OutboxSummary, ob)
	}
	if err := obRows.Err(); err != nil {
		return nil, err
	}

	return report, nil
}
