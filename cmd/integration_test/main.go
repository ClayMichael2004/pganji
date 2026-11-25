package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type AuthResp struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

func main() {
	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://localhost:8080/api/v1"

	fmt.Println("=== RUNNING AUTOMATED E2E INTEGRATION AUDIT ===")

	// 1. Pre-flight Health Check
	healthResp, err := client.Get("http://localhost:8080/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: API server unreachable on http://localhost:8080: %v\n", err)
		os.Exit(1)
	}
	healthResp.Body.Close()

	// 2. Dynamic User Registration
	uniqueEmail := fmt.Sprintf("audit-%d@pganji.io", time.Now().UnixNano())
	regBody, _ := json.Marshal(map[string]string{
		"email":     uniqueEmail,
		"password":  "AuditPassword2026!",
		"full_name": "Automated Auditor",
	})

	resp, err := client.Post(baseURL+"/auth/register", "application/json", bytes.NewBuffer(regBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Registration error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Registration failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var authData AuthResp
	if err := json.NewDecoder(resp.Body).Decode(&authData); err != nil {
		fmt.Fprintf(os.Stderr, "JSON decode error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[PASS] User Registration & Token Issuance (%s)\n", authData.UserID)

	// 3. Unauthenticated Guard Verification (Expect 401)
	dummyAcc := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	unauthResp, err := client.Get(fmt.Sprintf("%s/accounts/statement?account_id=%s", baseURL, dummyAcc))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unauthenticated request failed: %v\n", err)
		os.Exit(1)
	}
	if unauthResp.StatusCode != http.StatusUnauthorized {
		unauthResp.Body.Close()
		fmt.Fprintf(os.Stderr, "Expected 401 Unauthorized, got %d\n", unauthResp.StatusCode)
		os.Exit(1)
	}
	unauthResp.Body.Close()
	fmt.Println("[PASS] Unauthenticated Guard Protection (401 Unauthorized)")

	// 4. Authenticated Ownership Isolation (Expect 403)
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/accounts/statement?account_id=%s", baseURL, dummyAcc), nil)
	req.Header.Set("Authorization", "Bearer "+authData.Token)
	forbiddenResp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Forbidden isolation test failed: %v\n", err)
		os.Exit(1)
	}
	if forbiddenResp.StatusCode != http.StatusForbidden {
		forbiddenResp.Body.Close()
		fmt.Fprintf(os.Stderr, "Expected 403 Forbidden, got %d\n", forbiddenResp.StatusCode)
		os.Exit(1)
	}
	forbiddenResp.Body.Close()
	fmt.Println("[PASS] Tenant Ownership Isolation (403 Forbidden)")

	fmt.Println("\n>>> VERDICT: ALL E2E CONTRACTS VALIDATED WITH ZERO LOOPHOLES <<<")
}