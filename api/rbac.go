package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// RequireAdmin verifies in PostgreSQL that the authenticated user holds the ADMIN role.
func (s *Server) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := r.Context().Value(UserContextKey).(string)
		if !ok || userID == "" {
			ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var role string
		err := s.Pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id::text = $1`, userID).Scan(&role)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				ErrorResponse(w, http.StatusUnauthorized, "user not found")
				return
			}
			ErrorResponse(w, http.StatusInternalServerError, "failed to verify user role")
			return
		}

		if role != "ADMIN" {
			ErrorResponse(w, http.StatusForbidden, "forbidden: ADMIN role required to access treasury audit endpoints")
			return
		}

		next.ServeHTTP(w, r)
	})
}
