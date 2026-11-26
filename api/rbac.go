package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// RequireAdmin verifies in PostgreSQL that the authenticated user holds the ADMIN role.
func (s *Server) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := UserIDFromContext(r.Context())
		if userID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		var role string
		err := s.Pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id::text = $1`, userID).Scan(&role)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "user not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to verify user role"})
			return
		}

		if role != "ADMIN" {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "forbidden: ADMIN role required to access treasury audit endpoints",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}
