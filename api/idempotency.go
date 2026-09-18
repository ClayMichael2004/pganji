package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
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
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
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

			// Read & duplicate request body
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
				return
			}
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			hash := sha256.Sum256(bodyBytes)
			reqHash := hex.EncodeToString(hash[:])

			ctx := r.Context()

			// Check for existing key
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
						"error": "idempotency key reused with mismatched payload",
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

			// Insert pending execution entry
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
					"error": "concurrent request detected for idempotency key",
				})
				return
			}

			// Wrap writer to record the downstream response
			recorder := &responseRecorder{
				ResponseWriter: w,
				statusCode:     0,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(recorder, r)

			if recorder.statusCode == 0 {
				recorder.statusCode = http.StatusOK
			}

			// Save final execution response
			updateQuery := `
				UPDATE idempotency_keys 
				SET response_code = $1, response_body = $2 
				WHERE key = $3;
			`
			_, _ = pool.Exec(ctx, updateQuery, recorder.statusCode, recorder.body.Bytes(), key)
		})
	}
}