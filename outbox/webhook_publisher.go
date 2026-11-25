package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type WebhookPublisher struct {
	TargetURL string
	Secret    string
	Client    *http.Client
}

type webhookEnvelope struct {
	ID            string          `json:"id"`
	EventType     string          `json:"event_type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	CreatedAt     time.Time       `json:"created_at"`
	Data          json.RawMessage `json:"data"`
}

func NewWebhookPublisher(targetURL, secret string) *WebhookPublisher {
	return &WebhookPublisher{
		TargetURL: targetURL,
		Secret:    secret,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (p *WebhookPublisher) Publish(ctx context.Context, event Event) error {
	envelope := webhookEnvelope{
		ID:            event.ID,
		EventType:     event.EventType,
		AggregateType: event.AggregateType,
		AggregateID:   event.AggregateID,
		CreatedAt:     event.CreatedAt,
		Data:          event.Payload,
	}

	bodyBytes, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook envelope: %w", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := ComputeHMACSignature(p.Secret, timestamp, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TargetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to construct webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pganji-Outbox-Dispatcher/1.0")
	req.Header.Set("X-Pganji-Event-ID", event.ID)
	req.Header.Set("X-Pganji-Event-Type", event.EventType)
	req.Header.Set("X-Pganji-Timestamp", timestamp)
	req.Header.Set("X-Pganji-Signature", "sha256="+signature)

	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Drain body so the underlying TCP connection can be reused by http.Client
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook target returned non-2xx status: %d", resp.StatusCode)
	}

	slog.Info("webhook event delivered",
		"event_id", event.ID,
		"event_type", event.EventType,
		"target_url", p.TargetURL,
		"status_code", resp.StatusCode,
	)
	return nil
}

// ComputeHMACSignature generates a hex-encoded HMAC-SHA256 over "<timestamp>.<payload>".
func ComputeHMACSignature(secret, timestamp string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature validates an incoming signature in constant time (preventing timing attacks).
func VerifyWebhookSignature(secret, timestamp, signatureHeader string, payload []byte, maxSkew time.Duration) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}

	age := time.Since(time.Unix(ts, 0))
	if age < -maxSkew || age > maxSkew {
		return false
	}

	expected := "sha256=" + ComputeHMACSignature(secret, timestamp, payload)
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}
