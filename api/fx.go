package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"pganji/db"
)

type CreateAccountRequest struct {
	Currency string `json:"currency"` // "KES" or "USD"
}

type FXConvertRequest struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	SourceAmount  int64  `json:"source_amount"`
	Reference     string `json:"reference"`
	Description   string `json:"description"`
}

func (s *Server) HandleCreateAccount(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req CreateAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	acc, err := db.CreateWalletAccount(r.Context(), s.Pool, userID, req.Currency)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported currency") {
			ErrorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		ErrorResponse(w, http.StatusInternalServerError, "failed to create currency wallet")
		return
	}

	JSONResponse(w, http.StatusCreated, acc)
}

func (s *Server) HandleListAccounts(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	accounts, err := db.ListUserAccounts(r.Context(), s.Pool, userID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "failed to list user accounts")
		return
	}

	JSONResponse(w, http.StatusOK, map[string]any{
		"accounts": accounts,
		"count":    len(accounts),
	})
}

func (s *Server) HandleFXConvert(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req FXConvertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.FromAccountID) == "" || strings.TrimSpace(req.ToAccountID) == "" {
		ErrorResponse(w, http.StatusBadRequest, "from_account_id and to_account_id are required")
		return
	}
	if strings.TrimSpace(req.Reference) == "" {
		ErrorResponse(w, http.StatusBadRequest, "reference is required")
		return
	}
	if req.SourceAmount <= 0 {
		ErrorResponse(w, http.StatusBadRequest, "source_amount must be greater than zero")
		return
	}

	res, err := db.ConvertCurrencyTx(r.Context(), s.Pool, db.FXConvertParams{
		UserID:        userID,
		FromAccountID: req.FromAccountID,
		ToAccountID:   req.ToAccountID,
		SourceAmount:  req.SourceAmount,
		Reference:     req.Reference,
		Description:   req.Description,
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not found"):
			ErrorResponse(w, http.StatusNotFound, msg)
		case strings.Contains(msg, "access denied"):
			ErrorResponse(w, http.StatusForbidden, msg)
		case strings.Contains(msg, "insufficient"):
			ErrorResponse(w, http.StatusUnprocessableEntity, msg)
		case strings.Contains(msg, "duplicate key"), strings.Contains(msg, "unique"):
			ErrorResponse(w, http.StatusConflict, "FX conversion with this reference already exists")
		case strings.Contains(msg, "unsupported FX"), strings.Contains(msg, "distinct currencies"), strings.Contains(msg, "must be different"), strings.Contains(msg, "too small"):
			ErrorResponse(w, http.StatusBadRequest, msg)
		default:
			ErrorResponse(w, http.StatusInternalServerError, "FX conversion failed")
		}
		return
	}

	JSONResponse(w, http.StatusCreated, res)
}
