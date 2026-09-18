package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"pganji/auth"
)

type RegisterRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type AuthResponse struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

func (s *Server) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request payload"})
		return
	}

	if req.Email == "" || len(req.Password) < 8 || req.FullName == "" {
		JSONResponse(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "email, full_name, and password (minimum 8 characters) are required",
		})
		return
	}

	hashedPassword, err := auth.HashPassword(req.Password)
	if err != nil {
		JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to secure password"})
		return
	}

	var userID string
	query := `
		INSERT INTO users (email, password_hash, full_name)
		VALUES ($1, $2, $3)
		RETURNING id;
	`
	err = s.Pool.QueryRow(r.Context(), query, req.Email, hashedPassword, req.FullName).Scan(&userID)
	if err != nil {
		JSONResponse(w, http.StatusConflict, map[string]string{"error": "user with this email already exists"})
		return
	}

	token, err := auth.GenerateToken(userID, req.Email)
	if err != nil {
		JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to issue authentication token"})
		return
	}

	JSONResponse(w, http.StatusCreated, AuthResponse{
		Token:  token,
		UserID: userID,
		Email:  req.Email,
	})
}

func (s *Server) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request payload"})
		return
	}

	var (
		userID       string
		passwordHash string
	)

	query := `SELECT id, password_hash FROM users WHERE email = $1`
	err := s.Pool.QueryRow(r.Context(), query, req.Email).Scan(&userID, &passwordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
			return
		}
		JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "database query failed"})
		return
	}

	if !auth.CheckPasswordHash(req.Password, passwordHash) {
		JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}

	token, err := auth.GenerateToken(userID, req.Email)
	if err != nil {
		JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to issue authentication token"})
		return
	}

	JSONResponse(w, http.StatusOK, AuthResponse{
		Token:  token,
		UserID: userID,
		Email:  req.Email,
	})
}