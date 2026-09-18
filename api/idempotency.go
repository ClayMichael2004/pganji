package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
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

			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
				return
			}
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			hash := sha256.Sum256(bodyBytes)
			reqHash := hex.EncodeToString(hash[:])
			ctx := r.Context()

			// 1. Check existing record
			var (
				cachedCode  sql.NullInt32
				cachedBody  []byte
				storedHash  string
				lockedUntil time.Time
			)

			query := `SELECT request_hash, response_code, response_body, locked_until 
			          FROM idempotency_keys WHERE key = $1`
			err = pool.QueryRow(ctx, query, key).Scan(&storedHash, &cachedCode, &cachedBody, &lockedUntil)

			if err == nil {
				if storedHash != reqHash {
					JSONResponse(w, http.StatusUnprocessableEntity, map[string]string{
						"error": "idempotency key reused with different request payload",
					})
					return
				}
				if cachedCode.Valid && cachedCode.Int32 > 0 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Cache-Lookup", "HIT")
					w.WriteHeader(int(cachedCode.Int32))
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

			// 2. Lock row
			lockQuery := `
				INSERT INTO idempotency_keys (key, request_path, request_hash, locked_until)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (key) DO UPDATE 
				SET locked_until = $4
				WHERE idempotency_keys.response_code IS NULL;
			`
			_, err = pool.Exec(ctx, lockQuery, key, r.URL.Path, reqHash, time.Now().Add(30*time.Second))
			if err != nil {
				JSONResponse(w, http.StatusConflict, map[string]string{
					"error": "concurrent request detected for this idempotency key",
				})
				return
			}

			// 3. Intercept execution
			rec := &responseRecorder{
				ResponseWriter: w,
				statusCode:     0,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(rec, r)

			if rec.statusCode == 0 {
				rec.statusCode = http.StatusOK
			}

			// 4. Flush captured buffer to actual client socket
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.Header().Set("X-Cache-Lookup", "MISS")
			w.WriteHeader(rec.statusCode)
			w.Write(rec.body.Bytes())

			// 5. Save cached response to database
			var rawJSON any = nil
			if json.Valid(rec.body.Bytes()) {
				rawJSON = rec.body.String()
			}

			updateQuery := `
				UPDATE idempotency_keys 
				SET response_code = $1, response_body = $2::jsonb 
				WHERE key = $3;
			`
			_, updateErr := pool.Exec(ctx, updateQuery, rec.statusCode, rawJSON, key)
			if updateErr != nil {
				slog.Error("failed to persist idempotency response", "err", updateErr, "key", key)
			}
		})
	}
}