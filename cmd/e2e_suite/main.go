package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	baseURL       = "http://localhost:8080/api/v1"
	defaultSecret = "whsec_pganji_local_signing_secret_2026"
	fxBridgeKESID = "00000000-0000-0000-0000-000000000002"
	fxBridgeUSDID = "00000000-0000-0000-0000-000000000003"
)

type apiClient struct {
	httpClient    *http.Client
	token         string
	webhookSecret string
}

func (c *apiClient) doJSON(method, path string, body any, extraHeaders map[string]string) (int, []byte, error) {
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}

	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(rawBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBytes, nil
}

func (c *apiClient) signPayload(payload []byte) (string, string) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write([]byte(ts + "." + string(payload)))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return ts, sig
}

func assert(cond bool, msg string) {
	if !cond {
		fmt.Fprintf(os.Stderr, "[FAIL] %s\n", msg)
		os.Exit(1)
	}
	fmt.Printf("[PASS] %s\n", msg)
}

func getPoolNetBalance(ctx context.Context, pool *pgxpool.Pool, accountID string) int64 {
	var bal int64
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE -amount END), 0)
		FROM journal_entries
		WHERE account_id = $1
	`, accountID).Scan(&bal)
	return bal
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		home, _ := os.UserHomeDir()
		socketDir := filepath.Join(home, "my_postgres", "sockets")
		dbURL = fmt.Sprintf("postgres:///pganji?host=%s&port=5433", socketDir)
	}

	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		secret = defaultSecret
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to PostgreSQL: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	client := &apiClient{
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		webhookSecret: secret,
	}

	runTag := fmt.Sprintf("%d", time.Now().UnixNano())
	fmt.Printf("=== PGANJI ENTERPRISE E2E VERIFICATION SUITE (run=%s) ===\n\n", runTag)

	// -------------------------------------------------------------------------
	// 1. Register User & Provision Multi-Currency Wallets (2x KES, 1x USD)
	// -------------------------------------------------------------------------
	status, raw, err := client.doJSON("POST", "/auth/register", map[string]string{
		"email":     fmt.Sprintf("e2e-%s@pganji.io", runTag),
		"password":  "Password123!",
		"full_name": "E2E Suite Runner",
	}, nil)
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Register test user (HTTP %d)", status))

	var authResp struct {
		Token  string `json:"token"`
		UserID string `json:"user_id"`
	}
	_ = json.Unmarshal(raw, &authResp)
	client.token = authResp.Token
	assert(client.token != "", "Received non-empty JWT bearer token")

	createWallet := func(currency string) string {
		st, body, e := client.doJSON("POST", "/accounts", map[string]string{"currency": currency}, nil)
		assert(e == nil && st == http.StatusCreated, fmt.Sprintf("Provision %s wallet (HTTP %d)", currency, st))
		var w struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &w)
		return w.ID
	}

	kesPrimaryID := createWallet("KES")
	kesSecondaryID := createWallet("KES")
	usdWalletID := createWallet("USD")

	status, raw, err = client.doJSON("GET", "/accounts", nil, nil)
	var listResp struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(raw, &listResp)
	assert(err == nil && status == http.StatusOK && listResp.Count == 3, fmt.Sprintf("List multi-currency wallets count=%d", listResp.Count))

	// -------------------------------------------------------------------------
	// 2. HMAC-Signed C2B Deposit + Tampered Signature Rejection
	// -------------------------------------------------------------------------
	depPayload, _ := json.Marshal(map[string]any{
		"account_id":         kesPrimaryID,
		"amount":             500000, // KES 5,000.00
		"external_reference": "C2B-E2E-" + runTag,
		"phone_number":       "254712345678",
		"description":        "E2E C2B Deposit",
	})

	badStatus, _, _ := client.doJSON("POST", "/webhooks/mpesa/c2b", json.RawMessage(depPayload), map[string]string{
		"X-Pganji-Timestamp": fmt.Sprintf("%d", time.Now().Unix()),
		"X-Pganji-Signature": "sha256=0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert(badStatus == http.StatusUnauthorized, fmt.Sprintf("Reject invalid HMAC webhook signature (HTTP %d)", badStatus))

	ts, sig := client.signPayload(depPayload)
	status, raw, err = client.doJSON("POST", "/webhooks/mpesa/c2b", json.RawMessage(depPayload), map[string]string{
		"X-Pganji-Timestamp": ts,
		"X-Pganji-Signature": sig,
	})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Settle HMAC-signed C2B deposit of 500,000 KES cents (HTTP %d)", status))

	// -------------------------------------------------------------------------
	// 3. Cross-Currency FX Conversion (129,000 KES cents -> 1,000 USD cents)
	// -------------------------------------------------------------------------
	status, raw, err = client.doJSON("POST", "/fx/convert", map[string]any{
		"from_account_id": kesPrimaryID,
		"to_account_id":   usdWalletID,
		"source_amount":   129000,
		"reference":       "FX-E2E-" + runTag,
		"description":     "E2E KES to USD FX Conversion",
	}, map[string]string{"Idempotency-Key": "idem-fx-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Execute cross-currency KES->USD FX conversion (HTTP %d)", status))

	var fxResp struct {
		TransactionID string `json:"transaction_id"`
		TargetAmount  int64  `json:"target_amount"`
	}
	_ = json.Unmarshal(raw, &fxResp)
	assert(fxResp.TargetAmount == 1000, fmt.Sprintf("Exact rational FX conversion output: %d USD cents (expected 1000)", fxResp.TargetAmount))

	// -------------------------------------------------------------------------
	// 4. Internal Transfer + Compensating Reversal + Double-Reversal Guard
	// -------------------------------------------------------------------------
	txRef := "TX-E2E-" + runTag
	status, _, err = client.doJSON("POST", "/transfers", map[string]any{
		"from_account_id": kesPrimaryID,
		"to_account_id":   kesSecondaryID,
		"amount":          10000,
		"reference":       txRef,
		"description":     "E2E Transfer to Reverse",
	}, map[string]string{"Idempotency-Key": "idem-tx-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Execute internal KES transfer (HTTP %d)", status))

	var origTxID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM transactions WHERE reference = $1`, txRef).Scan(&origTxID)
	assert(err == nil && origTxID != "", "Resolved original transaction UUID")

	status, _, err = client.doJSON("POST", "/transfers/reverse", map[string]any{
		"transaction_id": origTxID,
		"reference":      "REV-" + txRef,
		"reason":         "E2E Reversal Verification",
	}, map[string]string{"Idempotency-Key": "idem-rev1-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Execute append-only compensating reversal (HTTP %d)", status))

	dupStatus, _, _ := client.doJSON("POST", "/transfers/reverse", map[string]any{
		"transaction_id": origTxID,
		"reference":      "REV-DUP-" + txRef,
		"reason":         "Duplicate reversal attempt",
	}, map[string]string{"Idempotency-Key": "idem-rev2-" + runTag})
	assert(dupStatus == http.StatusConflict, fmt.Sprintf("Block duplicate reversal of same transaction (HTTP %d)", dupStatus))

	// -------------------------------------------------------------------------
	// 5. B2C Withdrawal + Failed Callback Auto-Reversal
	// -------------------------------------------------------------------------
	b2cRef := "B2C-E2E-" + runTag
	status, _, err = client.doJSON("POST", "/clearing/withdraw", map[string]any{
		"account_id":         kesPrimaryID,
		"amount":             7500,
		"external_reference": b2cRef,
		"phone_number":       "254712345678",
		"description":        "E2E B2C Payout",
	}, map[string]string{"Idempotency-Key": "idem-wdr-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Initiate B2C withdrawal of 7,500 KES cents (HTTP %d)", status))

	cbPayload, _ := json.Marshal(map[string]any{
		"Result": map[string]any{
			"OriginatorConversationID": b2cRef,
			"TransactionID":            "QKL" + runTag,
			"ResultCode":               2001,
			"ResultDesc":               "Simulated Daraja initiator failure",
		},
	})
	cbTS, cbSig := client.signPayload(cbPayload)
	status, raw, err = client.doJSON("POST", "/webhooks/mpesa/b2c/result", json.RawMessage(cbPayload), map[string]string{
		"X-Pganji-Timestamp": cbTS,
		"X-Pganji-Signature": cbSig,
	})
	assert(err == nil && status == http.StatusOK, fmt.Sprintf("Process failed B2C callback & auto-reversal (HTTP %d)", status))

	// Expected KES primary balance: 500,000 (deposit) - 129,000 (FX) = 371,000 cents
	status, raw, _ = client.doJSON("GET", "/accounts/balance?account_id="+kesPrimaryID, nil, nil)
	var balResp struct {
		Balance int64 `json:"balance"`
	}
	_ = json.Unmarshal(raw, &balResp)
	assert(status == http.StatusOK && balResp.Balance == 371000, fmt.Sprintf("Primary KES wallet restored to 371,000 cents after B2C reversal (actual=%d)", balResp.Balance))

	// -------------------------------------------------------------------------
	// 6. 4-Leg Cross-Currency FX Reversal & Bridge Unwind Verification
	// -------------------------------------------------------------------------
	bridgeKESPre := getPoolNetBalance(ctx, pool, fxBridgeKESID)
	bridgeUSDPre := getPoolNetBalance(ctx, pool, fxBridgeUSDID)

	status, raw, err = client.doJSON("POST", "/fx/convert", map[string]any{
		"from_account_id": kesPrimaryID,
		"to_account_id":   usdWalletID,
		"source_amount":   129000,
		"reference":       "FX-REV-TARGET-" + runTag,
		"description":     "Second FX conversion to test 4-leg reversal",
	}, map[string]string{"Idempotency-Key": "idem-fx2-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Execute second KES->USD FX conversion (HTTP %d)", status))

	var fx2Resp struct {
		TransactionID string `json:"transaction_id"`
	}
	_ = json.Unmarshal(raw, &fx2Resp)

	status, _, err = client.doJSON("POST", "/transfers/reverse", map[string]any{
		"transaction_id": fx2Resp.TransactionID,
		"reference":      "REV-FX-" + runTag,
		"reason":         "E2E 4-leg FX bridge unwind test",
	}, map[string]string{"Idempotency-Key": "idem-rev-fx-" + runTag})
	assert(err == nil && status == http.StatusCreated, fmt.Sprintf("Reverse 4-leg KES->USD FX transaction (HTTP %d)", status))

	bridgeKESPost := getPoolNetBalance(ctx, pool, fxBridgeKESID)
	bridgeUSDPost := getPoolNetBalance(ctx, pool, fxBridgeUSDID)
	assert(bridgeKESPre == bridgeKESPost && bridgeUSDPre == bridgeUSDPost,
		fmt.Sprintf("FX bridge pools unwound cleanly (KES: %d->%d, USD: %d->%d)", bridgeKESPre, bridgeKESPost, bridgeUSDPre, bridgeUSDPost))

	// Verify USD clawback guard: transfer remaining 1,000 USD cents out, then attempt to reverse first FX trade
	usdSecondID := createWallet("USD")
	status, _, _ = client.doJSON("POST", "/transfers", map[string]any{
		"from_account_id": usdWalletID,
		"to_account_id":   usdSecondID,
		"amount":          1000,
		"reference":       "TX-USD-DRAIN-" + runTag,
		"description":     "Move USD before attempting FX reversal",
	}, map[string]string{"Idempotency-Key": "idem-usd-drain-" + runTag})
	assert(status == http.StatusCreated, "Transfer 1,000 USD cents to secondary USD wallet")

	clawbackStatus, _, _ := client.doJSON("POST", "/transfers/reverse", map[string]any{
		"transaction_id": fxResp.TransactionID,
		"reference":      "REV-FX-CLAWBACK-" + runTag,
		"reason":         "Attempt FX reversal after spending USD",
	}, map[string]string{"Idempotency-Key": "idem-rev-fx-clawback-" + runTag})
	assert(clawbackStatus == http.StatusUnprocessableEntity, fmt.Sprintf("Block FX reversal when USD clawback funds were spent (HTTP %d)", clawbackStatus))

	// -------------------------------------------------------------------------
	// 7. Database-Layer Immutability Trigger Verification
	// -------------------------------------------------------------------------
	_, updErr := pool.Exec(ctx, `UPDATE journal_entries SET amount = amount + 1 WHERE seq = 1`)
	assert(updErr != nil && strings.Contains(updErr.Error(), "ledger immutability violation"), "PostgreSQL trigger blocked direct UPDATE on journal_entries")

	_, delErr := pool.Exec(ctx, `DELETE FROM journal_entries WHERE seq = 1`)
	assert(delErr != nil && strings.Contains(delErr.Error(), "ledger immutability violation"), "PostgreSQL trigger blocked direct DELETE on journal_entries")

	// -------------------------------------------------------------------------
	// 8. RBAC Security Gate on Treasury Audit Endpoints (USER -> 403, ADMIN -> 200)
	// -------------------------------------------------------------------------
	unauthAuditStatus, _, _ := client.doJSON("GET", "/audit/verify-chain", nil, nil)
	assert(unauthAuditStatus == http.StatusForbidden, fmt.Sprintf("Block non-admin user from /audit/verify-chain (HTTP %d)", unauthAuditStatus))

	unauthRecStatus, _, _ := client.doJSON("GET", "/audit/reconcile", nil, nil)
	assert(unauthRecStatus == http.StatusForbidden, fmt.Sprintf("Block non-admin user from /audit/reconcile (HTTP %d)", unauthRecStatus))

	_, err = pool.Exec(ctx, `UPDATE users SET role = 'ADMIN' WHERE id::text = $1`, authResp.UserID)
	assert(err == nil, "Promoted E2E user to ADMIN role")

	// -------------------------------------------------------------------------
	// 9. Cryptographic SHA-256 Hash Chain Verification
	// -------------------------------------------------------------------------
	time.Sleep(250 * time.Millisecond)
	status, raw, err = client.doJSON("GET", "/audit/verify-chain", nil, nil)
	var chainResp struct {
		Status          string `json:"status"`
		TotalEntries    int64  `json:"total_entries"`
		TrialBalanceNet int64  `json:"trial_balance_net"`
	}
	_ = json.Unmarshal(raw, &chainResp)
	assert(err == nil && status == http.StatusOK && chainResp.Status == "VALID" && chainResp.TrialBalanceNet == 0,
		fmt.Sprintf("SHA-256 hash chain VALID across %d entries (HTTP %d, trial_balance_net=%d, body=%s)", chainResp.TotalEntries, status, chainResp.TrialBalanceNet, string(raw)))

	// -------------------------------------------------------------------------
	// 10. Multi-Currency Treasury & Clearing Reconciliation
	// -------------------------------------------------------------------------
	status, raw, err = client.doJSON("GET", "/audit/reconcile", nil, nil)
	var recResp struct {
		OverallStatus          string `json:"overall_status"`
		ClearingReconciliation struct {
			ClearingDiscrepancy int64 `json:"clearing_discrepancy"`
			Reconciled          bool  `json:"reconciled"`
		} `json:"clearing_reconciliation"`
	}
	_ = json.Unmarshal(raw, &recResp)
	assert(err == nil && status == http.StatusOK && recResp.OverallStatus == "RECONCILED" && recResp.ClearingReconciliation.Reconciled,
		fmt.Sprintf("Treasury reconciliation RECONCILED (clearing_discrepancy=%d)", recResp.ClearingReconciliation.ClearingDiscrepancy))

	fmt.Println("\n>>> VERDICT: ALL 10 ENTERPRISE LEDGER INVARIANTS PASSED <<<")
}
