package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pganji/db"
)

func TestIdempotencyConcurrentRaceProtection(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		home, _ := os.UserHomeDir()
		socketDir := filepath.Join(home, "my_postgres", "sockets")
		dbURL = fmt.Sprintf("postgres:///pganji?host=%s&port=5433", socketDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := db.ConnectDB(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping test: database connection failed: %v", err)
	}
	defer pool.Close()

	key := fmt.Sprintf("race-test-key-%d", time.Now().UnixNano())
	payload := []byte(`{"action":"transfer","amount":100}`)

	var handlerExecutions int64

	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&handlerExecutions, 1)
		time.Sleep(50 * time.Millisecond) // Simulate business execution delay
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	idempMiddleware := IdempotencyMiddleware(pool)
	protectedHandler := idempMiddleware(targetHandler)

	concurrency := 10
	var (
		wg            sync.WaitGroup
		status200     int64
		statusConflict int64
	)

	// Launch concurrent requests with the identical idempotency key
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewReader(payload))
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			protectedHandler.ServeHTTP(rec, req)

			switch rec.Code {
			case http.StatusOK:
				atomic.AddInt64(&status200, 1)
			case http.StatusConflict:
				atomic.AddInt64(&statusConflict, 1)
			}
		}()
	}

	wg.Wait()

	// Exactly ONE request must execute downstream handler; others must be blocked with 409 Conflict
	if handlerExecutions != 1 {
		t.Errorf("expected downstream handler to execute exactly once, but executed %d times", handlerExecutions)
	}
	if status200 != 1 {
		t.Errorf("expected exactly 1 HTTP 200 OK, got %d", status200)
	}
	if statusConflict != int64(concurrency-1) {
		t.Errorf("expected %d HTTP 409 Conflict responses, got %d", concurrency-1, statusConflict)
	}
}
