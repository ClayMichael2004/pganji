package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	baseURL      = "http://localhost:8080/api/v1"
	aliceAccount = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	bobAccount   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	currency     = "KES"
)

type AuthResp struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
}

type BalanceResp struct {
	Balance int64 `json:"balance"`
}

type TransferReq struct {
	Reference  	string `json:"reference"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Description   string `json:"description"`
}

func main() {
	client := &http.Client{Timeout: 5 * time.Second}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = fmt.Sprintf("postgres:///pganji?host=%s/my_postgres/sockets&port=5433", os.Getenv("HOME"))
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	// 1. Dynamic Registration
	uniqueEmail := fmt.Sprintf("concurrency-runner-%d@pganji.io", time.Now().UnixNano())
	regBody, _ := json.Marshal(map[string]string{
		"email":     uniqueEmail,
		"password":  "HarnessSecure2026!",
		"full_name": "Concurrency Harness",
	})

	authResp, err := client.Post(baseURL+"/auth/register", "application/json", bytes.NewBuffer(regBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Is the API server running on port 8080? Error: %v\n", err)
		os.Exit(1)
	}
	var authData AuthResp
	_ = json.NewDecoder(authResp.Body).Decode(&authData)
	authResp.Body.Close()

	// 2. Assign BOTH Alice & Bob to the newly authenticated test runner
	_, err = pool.Exec(ctx, `
		INSERT INTO accounts (id, user_id, account_number, currency, type)
		VALUES 
			($1, $2, 'ACC-ALICE-CONC', 'KES', 'ASSET'),
			($3, $2, 'ACC-BOB-CONC', 'KES', 'ASSET')
		ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id;
	`, aliceAccount, authData.UserID, bobAccount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to map test accounts: %v\n", err)
		os.Exit(1)
	}

	// Wait 200ms to ensure rate-limiter bucket is fresh
	time.Sleep(200 * time.Millisecond)

	// 3. Pre-Test Balances
	alicePre, errA := getBalance(client, authData.Token, aliceAccount)
	bobPre, errB := getBalance(client, authData.Token, bobAccount)
	if errA != nil || errB != nil {
		fmt.Fprintf(os.Stderr, "Failed pre-test balance query: Alice err=%v, Bob err=%v\n", errA, errB)
		os.Exit(1)
	}

	fmt.Println("=== PRE-TEST BALANCES ===")
	fmt.Printf("Alice : %d cents\n", alicePre)
	fmt.Printf("Bob   : %d cents\n\n", bobPre)

	concurrency := 50
	amount := int64(1000)
	fmt.Printf("Launching %d concurrent goroutines sending %d cents each...\n\n", concurrency, amount)

	var (
		successCount  int64
		throttleCount int64
		errorCount    int64
		firstError    string
		firstCode     int
		onceErr       sync.Once
		wg            sync.WaitGroup
	)

	start := time.Now()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			idempKey := fmt.Sprintf("conc-key-%d-%d", time.Now().UnixNano(), idx)
			reqPayload, _ := json.Marshal(TransferReq{
				Reference : 	fmt.Sprintf("TX-CONC-%d-%d", time.Now().UnixNano(), idx),
				FromAccountID: aliceAccount,
				ToAccountID:   bobAccount,
				Amount:        amount,
				Currency:      currency,
				Description:   fmt.Sprintf("Concurrent transfer %d", idx),
			})
			req, _ := http.NewRequest("POST", baseURL+"/transfers", bytes.NewBuffer(reqPayload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+authData.Token)
			req.Header.Set("Idempotency-Key", idempKey)

			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer resp.Body.Close()

			switch resp.StatusCode {
			case http.StatusCreated:
				atomic.AddInt64(&successCount, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&throttleCount, 1)
			default:
				atomic.AddInt64(&errorCount, 1)
				bodyBytes, _ := io.ReadAll(resp.Body)
				onceErr.Do(func() {
					firstCode = resp.StatusCode
					firstError = string(bodyBytes)
				})
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	fmt.Printf("=== RUN COMPLETED IN %v ===\n", duration)
	fmt.Printf("Successful (201)  : %d\n", successCount)
	fmt.Printf("Throttled (429)   : %d\n", throttleCount)
	fmt.Printf("Other Errors      : %d\n", errorCount)
	if firstError != "" {
		fmt.Printf(">>> Sample Failure Details: HTTP %d: %s\n", firstCode, firstError)
	}
	fmt.Println()

	// Wait 500ms to let rate-limiter bucket replenish before auditing
	time.Sleep(500 * time.Millisecond)

	// 4. Audit & Reconciliation
	alicePost, _ := getBalance(client, authData.Token, aliceAccount)
	bobPost, _ := getBalance(client, authData.Token, bobAccount)
	expectedTransfer := successCount * amount

	fmt.Println("=== AUDIT & RECONCILIATION ===")
	fmt.Printf("Alice: %d -> %d (Delta: %d cents)\n", alicePre, alicePost, alicePost-alicePre)
	fmt.Printf("Bob  : %d -> %d (Delta: +%d cents)\n", bobPre, bobPost, bobPost-bobPre)
	fmt.Printf("Expected Transfer : %d cents\n\n", expectedTransfer)

	if (alicePre-alicePost) == expectedTransfer && (bobPost-bobPre) == expectedTransfer && errorCount == 0 {
		fmt.Println(">>> VERDICT: ZERO DISCREPANCY. LEDGER IS MATHEMATICALLY CONSISTENT <<<")
	} else {
		fmt.Println(">>> VERDICT: AUDIT FAILED <<<")
		os.Exit(1)
	}
}

func getBalance(client *http.Client, token, accountID string) (int64, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/accounts/balance?account_id=%s", baseURL, accountID), nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return -1, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	var data BalanceResp
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &data); err != nil {
		return -1, err
	}
	return data.Balance, nil
}