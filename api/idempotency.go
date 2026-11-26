package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *responseRecorder) Header() http.Header {
	return r.ResponseWriter.Header()
}

func IdempotencyMiddleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Read request body safely
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "unable to read request body"})
				return
			}
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			hash := sha256.Sum256(bodyBytes)
			reqHash := hex.EncodeToString(hash[:])
			ctx := r.Context()

			// 1. Atomic lock acquisition attempt
			// If key doesn't exist, insert with locked_until.
			// If key exists and is expired (locked_until < now) without response, acquire lock.
			// If already completed or actively locked, ON CONFLICT will not update and RowsAffected will be 0.
			lockQuery := `
				INSERT INTO idempotency_keys (key, request_path, request_hash, locked_until)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (key) DO UPDATE 
				SET locked_until = EXCLUDED.locked_until
				WHERE idempotency_keys.response_code IS NULL 
				  AND idempotency_keys.locked_until < CURRENT_TIMESTAMP;
			`
			tag, err := pool.Exec(ctx, lockQuery, key, r.URL.Path, reqHash, time.Now().Add(30*time.Second))
			if err != nil {
				slog.Error("failed to execute idempotency lock query", "err", err, "key", key)
				JSONResponse(w, http.StatusInternalServerError, map[string]string{
					"error": "internal idempotency error",
				})
				return
			}

			// If RowsAffected == 0, another request already owns this key or completed it
			if tag.RowsAffected() == 0 {
				var (
					storedHash  string
					cachedCode  *int
					cachedBody  []byte
					lockedUntil time.Time
				)

				query := `SELECT request_hash, response_code, response_body, locked_until 
				          FROM idempotency_keys WHERE key = $1`
				err = pool.QueryRow(ctx, query, key).Scan(&storedHash, &cachedCode, &cachedBody, &lockedUntil)
				if err != nil {
					slog.Error("error checking existing idempotency key", "err", err)
					JSONResponse(w, http.StatusInternalServerError, map[string]string{
						"error": "internal idempotency lookup failure",
					})
					return
				}

				if storedHash != reqHash {
					JSONResponse(w, http.StatusUnprocessableEntity, map[string]string{
						"error": "idempotency key reused with different request payload",
					})
					return
				}

				if cachedCode != nil && *cachedCode > 0 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Cache-Lookup", "HIT")
					w.WriteHeader(*cachedCode)
					w.Write(cachedBody)
					return
				}

				if time.Now().Before(lockedUntil) {
					JSONResponse(w, http.StatusConflict, map[string]string{
						"error": "request with this idempotency key is currently processing",
					})
					return
				}
			}

			// 2. Execute downstream handler through the recorder
			rec := &responseRecorder{
				ResponseWriter: w,
				statusCode:     0,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(rec, r)

			if rec.statusCode == 0 {
				rec.statusCode = http.StatusOK
			}

			// 4. Save response to database synchronously before responding
			var jsonBody *string
			if rec.body.Len() > 0 && json.Valid(rec.body.Bytes()) {
				str := rec.body.String()
				jsonBody = &str
			}

			updateQuery := `
				UPDATE idempotency_keys 
				SET response_code = $1, response_body = $2::jsonb, locked_until = CURRENT_TIMESTAMP
				WHERE key = $3;
			`
			_, updateErr := pool.Exec(ctx, updateQuery, rec.statusCode, jsonBody, key)
			if updateErr != nil {
				slog.Error("CRITICAL: failed to persist idempotency record", "err", updateErr, "key", key)
			}

			// 5. Send recorded output to client with MISS header
			for k, values := range rec.Header() {
				for _, v := range values {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("X-Cache-Lookup", "MISS")
			w.WriteHeader(rec.statusCode)
			w.Write(rec.body.Bytes())
		})
	}
}