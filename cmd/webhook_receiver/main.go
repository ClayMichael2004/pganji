package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"pganji/outbox"
)

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		secret = "whsec_pganji_local_signing_secret_2026"
	}

	http.HandleFunc("/webhooks/pganji", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		eventID := r.Header.Get("X-Pganji-Event-ID")
		eventType := r.Header.Get("X-Pganji-Event-Type")
		timestamp := r.Header.Get("X-Pganji-Timestamp")
		signature := r.Header.Get("X-Pganji-Signature")

		if !outbox.VerifyWebhookSignature(secret, timestamp, signature, body, 5*time.Minute) {
			slog.Warn("rejected webhook with invalid HMAC signature", "event_id", eventID)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		slog.Info("verified webhook received",
			"event_id", eventID,
			"event_type", eventType,
			"signature", signature[:20]+"...",
			"payload", string(body),
		)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	})

	fmt.Println("Webhook receiver listening on http://localhost:9090/webhooks/pganji")
	if err := http.ListenAndServe(":9090", nil); err != nil {
		slog.Error("receiver failed", "error", err)
		os.Exit(1)
	}
}
