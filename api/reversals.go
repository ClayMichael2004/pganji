package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"pganji/db"
)

type CreateReversalRequest struct {
	TransactionID string `json:"transaction_id"`
	Reference     string `json:"reference"`
	Reason        string `json:"reason"`
}

func (s *Server) HandleReverseTransaction(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req CreateReversalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.TransactionID) == "" {
		ErrorResponse(w, http.StatusBadRequest, "transaction_id is required")
		return
	}
	if strings.TrimSpace(req.Reference) == "" {
		ErrorResponse(w, http.StatusBadRequest, "reference is required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		ErrorResponse(w, http.StatusBadRequest, "reason is required")
		return
	}

	// Verify transaction exists and caller owns a participating account
	var exists bool
	if err := s.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM transactions WHERE id = $1)`, req.TransactionID).Scan(&exists); err != nil || !exists {
		ErrorResponse(w, http.StatusNotFound, "transaction not found")
		return
	}

	var isParticipant bool
	err := s.Pool.QueryRow(r.Context(), `
		SELECT EXISTS(
			SELECT 1
			FROM journal_entries j
			JOIN accounts a ON j.account_id = a.id
			WHERE j.transaction_id = $1 AND a.user_id::text = $2
		);
	`, req.TransactionID, userID).Scan(&isParticipant)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "failed to verify transaction ownership")
		return
	}
	if !isParticipant {
		ErrorResponse(w, http.StatusForbidden, "access denied: you are not a party to this transaction")
		return
	}

	result, err := db.ReverseTransactionTx(r.Context(), s.Pool, db.ReversalParams{
		OriginalTransactionID: req.TransactionID,
		Reference:             req.Reference,
		Reason:                req.Reason,
		InitiatedByUserID:     userID,
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not found"):
			ErrorResponse(w, http.StatusNotFound, msg)
		case strings.Contains(msg, "already reversed"), strings.Contains(msg, "cannot reverse"), strings.Contains(msg, "duplicate key"), strings.Contains(msg, "unique"):
			ErrorResponse(w, http.StatusConflict, msg)
		case strings.Contains(msg, "insufficient funds"):
			ErrorResponse(w, http.StatusUnprocessableEntity, msg)
		default:
			ErrorResponse(w, http.StatusInternalServerError, "transaction reversal failed")
		}
		return
	}

	JSONResponse(w, http.StatusCreated, result)
}
