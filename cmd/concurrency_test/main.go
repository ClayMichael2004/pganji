package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type TransferRequest struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        int64  `json:"amount"`
	Reference     string `json:"reference"`
	Description   string `json:"description"`
}

type BalanceResponse struct {
	AccountID string `json:"account_id"`
	Balance   int64  `json:"balance"`
	Formatted int64  `json:"formatted"`
}

func main() {
	aliceID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	bobID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Snapshot pre-test balances
	aliceInitial := fetchBalance(client, aliceID)
	bobInitial := fetchBalance(client, bobID)
	fmt.Printf("=== PRE-TEST BALANCES ===\n")
	fmt.Printf("Alice : %d cents\n", aliceInitial)
	fmt.Printf("Bob   : %d cents\n\n", bobInitial)

	totalRequests := 50
	transferAmount := int64(1000) // 10.00 KES (1,000 cents per transfer)

	var wg sync.WaitGroup
	var successCount int64
	var throttledCount int64
	var failedCount int64

	fmt.Printf("Launching %d concurrent goroutines sending %d cents each...\n", totalRequests, transferAmount)
	start := time.Now()

	for i := 1; i <= totalRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			reqBody := TransferRequest{
				FromAccountID: aliceID,
				ToAccountID:   bobID,
				Amount:        transferAmount,
				Reference:     fmt.Sprintf("CONCUR-TX-%d-%d", time.Now().UnixNano(), idx),
				Description:   fmt.Sprintf("Concurrent transfer test #%d", idx),
			}

			payload, _ := json.Marshal(reqBody)
			req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/api/v1/transfers", bytes.NewBuffer(payload))
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				return
			}

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", fmt.Sprintf("IDEMP-CONCUR-%d-%d", time.Now().UnixNano(), idx))

			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				return
			}
			defer resp.Body.Close()

			switch resp.StatusCode {
			case http.StatusCreated:
				atomic.AddInt64(&successCount, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&throttledCount, 1)
			default:
				atomic.AddInt64(&failedCount, 1)
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	// 2. Snapshot post-test balances
	aliceFinal := fetchBalance(client, aliceID)
	bobFinal := fetchBalance(client, bobID)

	expectedDeduction := successCount * transferAmount

	fmt.Printf("\n=== RUN COMPLETED IN %v ===\n", duration)
	fmt.Printf("Successful (201)  : %d\n", successCount)
	fmt.Printf("Throttled (429)   : %d\n", throttledCount)
	fmt.Printf("Other Errors      : %d\n", failedCount)

	fmt.Printf("\n=== AUDIT & RECONCILIATION ===\n")
	fmt.Printf("Alice: %d -> %d (Delta: -%d cents)\n", aliceInitial, aliceFinal, aliceInitial-aliceFinal)
	fmt.Printf("Bob  : %d -> %d (Delta: +%d cents)\n", bobInitial, bobFinal, bobFinal-bobInitial)
	fmt.Printf("Expected Transfer : %d cents\n", expectedDeduction)

	if (aliceInitial-aliceFinal) == expectedDeduction && (bobFinal-bobInitial) == expectedDeduction {
		fmt.Printf("\n>>> VERDICT: ZERO DISCREPANCY. LEDGER IS MATHEMATICALLY CONSISTENT <<<\n")
	} else {
		fmt.Printf("\n>>> VERDICT: RECONCILIATION FAILURE DETECTED <<<\n")
	}
}

func fetchBalance(client *http.Client, accountID string) int64 {
	url := fmt.Sprintf("http://localhost:8080/api/v1/accounts/balance?account_id=%s", accountID)
	resp, err := client.Get(url)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var b BalanceResponse
	if err := json.Unmarshal(body, &b); err != nil {
		return -1
	}
	return b.Balance
}