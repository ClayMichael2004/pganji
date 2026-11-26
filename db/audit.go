package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type LedgerAuditReport struct {
	VerifiedAt         time.Time `json:"verified_at"`
	Status             string    `json:"status"` // "VALID" or "COMPROMISED"
	TotalEntries       int64     `json:"total_entries"`
	GenesisHash        string    `json:"genesis_hash"`
	HeadHash           string    `json:"head_hash"`
	HeadSeq            int64     `json:"head_seq"`
	TotalDebits        int64     `json:"total_debits"`
	TotalCredits       int64     `json:"total_credits"`
	TrialBalanceNet    int64     `json:"trial_balance_net"`
	BrokenAtSeq        *int64    `json:"broken_at_seq,omitempty"`
	BrokenEntryID      string    `json:"broken_entry_id,omitempty"`
	FailureDescription string    `json:"failure_description,omitempty"`
}

// VerifyLedgerHashChain recomputes the full SHA-256 chain across all journal_entries and checks trial balance.
func VerifyLedgerHashChain(ctx context.Context, pool *pgxpool.Pool) (*LedgerAuditReport, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, seq, prev_hash, entry_hash, transaction_id, account_id, entry_type, amount
		FROM journal_entries
		ORDER BY seq ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query journal entries for audit: %w", err)
	}
	defer rows.Close()

	expectedPrev := strings.Repeat("0", 64)
	report := &LedgerAuditReport{
		VerifiedAt: time.Now().UTC(),
		Status:     "VALID",
	}

	var expectedSeq int64 = 1

	for rows.Next() {
		var (
			id, prevHash, entryHash, txID, accountID, entryType string
			seq, amount                                         int64
		)

		if err := rows.Scan(&id, &seq, &prevHash, &entryHash, &txID, &accountID, &entryType, &amount); err != nil {
			return nil, fmt.Errorf("failed to scan journal entry row: %w", err)
		}

		report.TotalEntries++

		if entryType == "DEBIT" {
			report.TotalDebits += amount
		} else if entryType == "CREDIT" {
			report.TotalCredits += amount
		}

		// 1. Check monotonic sequence continuity (detects deleted rows)
		if seq != expectedSeq {
			report.Status = "COMPROMISED"
			report.BrokenAtSeq = &seq
			report.BrokenEntryID = id
			report.FailureDescription = fmt.Sprintf("sequence gap detected: expected seq %d, found %d", expectedSeq, seq)
			return report, nil
		}

		// 2. Check link to previous block
		if prevHash != expectedPrev {
			report.Status = "COMPROMISED"
			report.BrokenAtSeq = &seq
			report.BrokenEntryID = id
			report.FailureDescription = fmt.Sprintf("prev_hash mismatch at seq %d: expected %s, got %s", seq, expectedPrev, prevHash)
			return report, nil
		}

		// 3. Recompute SHA-256 digest over canonical fields
		payload := fmt.Sprintf("%d|%s|%s|%s|%s|%d", seq, prevHash, txID, accountID, entryType, amount)
		sum := sha256.Sum256([]byte(payload))
		recomputedHash := hex.EncodeToString(sum[:])

		if recomputedHash != entryHash {
			report.Status = "COMPROMISED"
			report.BrokenAtSeq = &seq
			report.BrokenEntryID = id
			report.FailureDescription = fmt.Sprintf("SHA-256 payload tamper detected at seq %d: stored %s != recomputed %s", seq, entryHash, recomputedHash)
			return report, nil
		}

		if report.TotalEntries == 1 {
			report.GenesisHash = entryHash
		}
		report.HeadHash = entryHash
		report.HeadSeq = seq

		expectedPrev = entryHash
		expectedSeq++
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating journal entries: %w", err)
	}

	report.TrialBalanceNet = report.TotalDebits - report.TotalCredits
	if report.TrialBalanceNet != 0 {
		report.Status = "COMPROMISED"
		report.FailureDescription = fmt.Sprintf("double-entry trial balance imbalance: debits (%d) != credits (%d)", report.TotalDebits, report.TotalCredits)
	}

	return report, nil
}
