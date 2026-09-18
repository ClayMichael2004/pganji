package api

import (
	"context"
	"net/http"
	"strings"

	"pganji/auth"
)

type contextKey string

const (
	UserContextKey  contextKey = "authenticated_user_id"
	EmailContextKey contextKey = "authenticated_email"
)

func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": "missing authorization header"})
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": "invalid authorization format, expected Bearer <token>"})
			return
		}

		claims, err := auth.ValidateToken(parts[1])
		if err != nil {
			JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, claims.UserID)
		ctx = context.WithValue(ctx, EmailContextKey, claims.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}