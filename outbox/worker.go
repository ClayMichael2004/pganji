package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/metrics"
)

const (
	MaxRetries = 5
	BatchSize  = 25
)

type Event struct {
	ID            string          `json:"id"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
	RetryCount    int             `json:"retry_count"`
	CreatedAt     time.Time       `json:"created_at"`
}

type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

type LogPublisher struct{}

func (p *LogPublisher) Publish(ctx context.Context, event Event) error {
	slog.Info("outbox event dispatched",
		"event_id", event.ID,
		"aggregate_type", event.AggregateType,
		"aggregate_id", event.AggregateID,
		"event_type", event.EventType,
	)
	return nil
}

func InsertEvent(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, eventType string, payload any) error {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1, $2, $3, $4)
	`, aggregateType, aggregateID, eventType, rawPayload)
	if err != nil {
		return fmt.Errorf("failed to insert outbox event: %w", err)
	}

	return nil
}

type Worker struct {
	Pool         *pgxpool.Pool
	Publisher    Publisher
	PollInterval time.Duration
}

func NewWorker(pool *pgxpool.Pool, publisher Publisher, interval time.Duration) *Worker {
	if publisher == nil {
		publisher = &LogPublisher{}
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	return &Worker{
		Pool:         pool,
		Publisher:    publisher,
		PollInterval: interval,
	}
}

func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	slog.Info("transactional outbox worker started", "poll_interval", w.PollInterval.String())

	for {
		select {
		case <-ctx.Done():
			slog.Info("outbox worker shutting down")
			return
		case <-ticker.C:
			if err := w.processBatch(ctx); err != nil {
				slog.Error("failed to process outbox batch", "error", err)
			}
		}
	}
}

func (w *Worker) processBatch(ctx context.Context) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin outbox batch transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, payload, retry_count, created_at
		FROM outbox_events
		WHERE status = 'PENDING'
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, BatchSize)
	if err != nil {
		return fmt.Errorf("failed to fetch outbox events: %w", err)
	}

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType, &e.Payload, &e.RetryCount, &e.CreatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan outbox event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()

	if len(events) == 0 {
		return nil
	}

	for _, event := range events {
		pubErr := w.Publisher.Publish(ctx, event)
		if pubErr == nil {
			_, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET status = 'PUBLISHED', published_at = CURRENT_TIMESTAMP
				WHERE id = $1
			`, event.ID)
			if err != nil {
				return fmt.Errorf("failed to mark outbox event as published: %w", err)
			}
			metrics.OutboxEventsPublishedTotal.WithLabelValues(event.EventType).Inc()
			continue
		}

		metrics.OutboxEventsFailedTotal.WithLabelValues(event.EventType).Inc()
		nextRetryCount := event.RetryCount + 1
		if nextRetryCount >= MaxRetries {
			_, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET status = 'FAILED', retry_count = $2, last_error = $3
				WHERE id = $1
			`, event.ID, nextRetryCount, pubErr.Error())
			if err != nil {
				return fmt.Errorf("failed to mark outbox event %s as failed: %w", event.ID, err)
			}
			slog.Error("outbox event moved to dead-letter state", "event_id", event.ID, "error", pubErr)
		} else {
			_, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET retry_count = $2, last_error = $3
				WHERE id = $1
			`, event.ID, nextRetryCount, pubErr.Error())
			if err != nil {
				return fmt.Errorf("failed to update retry count for outbox event %s: %w", event.ID, err)
			}
		}
	}

	return tx.Commit(ctx)
}