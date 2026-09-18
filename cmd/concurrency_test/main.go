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
	Balance int64 `json:"balance"`
}

func main() {
	aliceID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	bobID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	totalTransfers := 50
	transferAmount := int64(10000) // 100.00 KES (10,000 cents)

	var wg sync.WaitGroup
	var successCount int64
	var failCount int64

	client := &http.Client{Timeout: 10 * time.Second}

	fmt.Printf("Starting %d concurrent transfers of %d cents...\n", totalTransfers, transferAmount)
	startTime := time.Now()

	for i := 1; i <= totalTransfers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			reqBody := TransferRequest{
				FromAccountID: aliceID,
				ToAccountID:   bobID,
				Amount:        transferAmount,
				Reference:     fmt.Sprintf("CONCUR-TX-%d-%d", time.Now().UnixNano(), idx),
				Description:   fmt.Sprintf("Concurrent stress test #%d", idx),
			}

			payload, _ := json.Marshal(reqBody)
			req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/api/v1/transfers", bytes.NewBuffer(payload))
			if err != nil {
				atomic.AddInt64(&failCount, 1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", fmt.Sprintf("IDEMP-KEY-%d-%d", time.Now().Unix(), idx))

			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&failCount, 1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusCreated {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&failCount, 1)
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(startTime)

	fmt.Printf("\nCompleted in %v\n", duration)
	fmt.Printf("Successful transfers: %d\n", successCount)
	fmt.Printf("Rejected/Throttled:   %d\n", failCount)

	// Fetch final balances
	aliceBal := fetchBalance(client, aliceID)
	bobBal := fetchBalance(client, bobID)

	fmt.Printf("\nFinal Balances:\n")
	fmt.Printf("Alice: %d cents (%.2f KES)\n", aliceBal, float64(aliceBal)/100.0)
	fmt.Printf("Bob:   %d cents (%.2f KES)\n", bobBal, float64(bobBal)/100.0)
}

func fetchBalance(client *http.Client, accountID string) int64 {
	url := fmt.Sprintf("http://localhost:8080/api/v1/account/balance?account_id=%s", accountID)
	resp, err := client.Get(url)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var b BalanceResponse
	_ = json.Unmarshal(body, &b)
	return b.Balance
}