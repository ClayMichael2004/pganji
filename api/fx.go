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

type CreateFXQuoteRequest struct {
	SourceCurrency string `json:"source_currency"`
	TargetCurrency string `json:"target_currency"`
	SourceAmount   int64  `json:"source_amount"`
}

type UpdateFXRateRequest struct {
	Pair      string `json:"pair"`       // e.g. "USD_KES"
	MidRate   string `json:"mid_rate"`   // e.g. "129.50000000"
	SpreadBps int    `json:"spread_bps"` // e.g. 100 for 1.00%
}

type FXConvertRequest struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	SourceAmount  int64  `json:"source_amount"`
	Reference     string `json:"reference"`
	Description   string `json:"description"`
	QuoteID       string `json:"quote_id,omitempty"`
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

// HandleListFXRates returns the real-time buy and sell currency rates with bid/ask spreads.
func (s *Server) HandleListFXRates(w http.ResponseWriter, r *http.Request) {
	rates, err := db.ListFXRates(r.Context(), s.Pool)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "failed to fetch FX rates")
		return
	}

	JSONResponse(w, http.StatusOK, map[string]any{
		"rates": rates,
		"count": len(rates),
	})
}

// HandleUpdateFXRate allows administrators / market makers to update live rates in real time.
func (s *Server) HandleUpdateFXRate(w http.ResponseWriter, r *http.Request) {
	var req UpdateFXRateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.Pair) == "" || strings.TrimSpace(req.MidRate) == "" {
		ErrorResponse(w, http.StatusBadRequest, "pair and mid_rate are required")
		return
	}

	updated, err := db.UpdateFXRate(r.Context(), s.Pool, req.Pair, req.MidRate, req.SpreadBps)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	JSONResponse(w, http.StatusOK, updated)
}

// HandleCreateFXQuote issues an RFQ guaranteed quote with a 30-second TTL.
func (s *Server) HandleCreateFXQuote(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req CreateFXQuoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.SourceCurrency) == "" || strings.TrimSpace(req.TargetCurrency) == "" {
		ErrorResponse(w, http.StatusBadRequest, "source_currency and target_currency are required")
		return
	}
	if req.SourceAmount <= 0 {
		ErrorResponse(w, http.StatusBadRequest, "source_amount must be greater than zero")
		return
	}

	quote, err := db.CreateFXQuote(r.Context(), s.Pool, userID, req.SourceCurrency, req.TargetCurrency, req.SourceAmount)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "unsupported FX"), strings.Contains(msg, "different"), strings.Contains(msg, "too small"):
			ErrorResponse(w, http.StatusBadRequest, msg)
		default:
			ErrorResponse(w, http.StatusInternalServerError, "failed to issue FX quote")
		}
		return
	}

	JSONResponse(w, http.StatusCreated, quote)
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
		QuoteID:       req.QuoteID,
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
		case strings.Contains(msg, "quote expired"), strings.Contains(msg, "quote is already"), strings.Contains(msg, "mismatch"):
			ErrorResponse(w, http.StatusUnprocessableEntity, msg)
		case strings.Contains(msg, "unsupported FX"), strings.Contains(msg, "distinct currencies"), strings.Contains(msg, "must be different"), strings.Contains(msg, "too small"):
			ErrorResponse(w, http.StatusBadRequest, msg)
		default:
			ErrorResponse(w, http.StatusInternalServerError, "FX conversion failed")
		}
		return
	}

	JSONResponse(w, http.StatusCreated, res)
}
