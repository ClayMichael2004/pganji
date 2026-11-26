package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"pganji/db"
	"pganji/outbox"
)

var kePhoneRegex = regexp.MustCompile(`^254(7|1)\d{8}$`)

type MpesaDepositWebhookRequest struct {
	AccountID         string `json:"account_id"`
	Amount            int64  `json:"amount"`
	ExternalReference string `json:"external_reference"` // e.g. M-Pesa Receipt Number "SJK8912PLM"
	PhoneNumber       string `json:"phone_number"`       // e.g. "254712345678"
	Description       string `json:"description"`
}

type WithdrawRequest struct {
	AccountID         string `json:"account_id"`
	Amount            int64  `json:"amount"`
	ExternalReference string `json:"external_reference"` // Client payout reference
	PhoneNumber       string `json:"phone_number"`       // Recipient MSISDN "2547XXXXXXXX"
	Description       string `json:"description"`
}

// HandleMpesaDepositWebhook processes HMAC-signed C2B / STK Push settlement callbacks.
func (s *Server) HandleMpesaDepositWebhook(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, "failed to read webhook body")
		return
	}
	defer r.Body.Close()

	// Verify HMAC-SHA256 signature if WEBHOOK_SECRET is configured
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret != "" {
		ts := r.Header.Get("X-Pganji-Timestamp")
		sig := r.Header.Get("X-Pganji-Signature")
		if !outbox.VerifyWebhookSignature(secret, ts, sig, bodyBytes, 5*time.Minute) {
			ErrorResponse(w, http.StatusUnauthorized, "invalid or expired webhook signature")
			return
		}
	}

	var req MpesaDepositWebhookRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.AccountID) == "" || strings.TrimSpace(req.ExternalReference) == "" {
		ErrorResponse(w, http.StatusBadRequest, "account_id and external_reference are required")
		return
	}
	if req.Amount <= 0 {
		ErrorResponse(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}
	if !kePhoneRegex.MatchString(req.PhoneNumber) {
		ErrorResponse(w, http.StatusBadRequest, "phone_number must be in 2547XXXXXXXX or 2541XXXXXXXX format")
		return
	}
	if req.Description == "" {
		req.Description = "M-Pesa C2B Deposit"
	}

	txID, err := db.DepositTx(r.Context(), s.Pool, db.ClearingParams{
		UserAccountID:     req.AccountID,
		Amount:            req.Amount,
		Provider:          "MPESA_C2B",
		ExternalReference: req.ExternalReference,
		PhoneNumber:       req.PhoneNumber,
		Description:       req.Description,
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			ErrorResponse(w, http.StatusConflict, "deposit with this external_reference already settled")
			return
		}
		if strings.Contains(err.Error(), "not found") {
			ErrorResponse(w, http.StatusNotFound, "target account not found")
			return
		}
		ErrorResponse(w, http.StatusInternalServerError, "failed to settle deposit")
		return
	}

	JSONResponse(w, http.StatusCreated, map[string]any{
		"status":             "SETTLED",
		"transaction_id":     txID,
		"external_reference": req.ExternalReference,
		"amount":             req.Amount,
	})
}

// HandleWithdraw initiates an authenticated B2C payout from a user's owned wallet account.
func (s *Server) HandleWithdraw(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(UserContextKey).(string)
	if !ok || userID == "" {
		ErrorResponse(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req WithdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if strings.TrimSpace(req.AccountID) == "" || strings.TrimSpace(req.ExternalReference) == "" {
		ErrorResponse(w, http.StatusBadRequest, "account_id and external_reference are required")
		return
	}
	if req.Amount <= 0 {
		ErrorResponse(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}
	if !kePhoneRegex.MatchString(req.PhoneNumber) {
		ErrorResponse(w, http.StatusBadRequest, "phone_number must be in 2547XXXXXXXX or 2541XXXXXXXX format")
		return
	}

	// Enforce tenant ownership before debiting funds
	var ownerID string
	err := s.Pool.QueryRow(r.Context(), `SELECT COALESCE(user_id::text, '') FROM accounts WHERE id = $1`, req.AccountID).Scan(&ownerID)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "account not found")
		return
	}
	if ownerID != userID {
		ErrorResponse(w, http.StatusForbidden, "access denied: account belongs to another user")
		return
	}

	if req.Description == "" {
		req.Description = "M-Pesa B2C Withdrawal"
	}

	txID, err := db.WithdrawTx(r.Context(), s.Pool, db.ClearingParams{
		UserAccountID:     req.AccountID,
		Amount:            req.Amount,
		Provider:          "MPESA_B2C",
		ExternalReference: req.ExternalReference,
		PhoneNumber:       req.PhoneNumber,
		Description:       req.Description,
	})
	if err != nil {
		if strings.Contains(err.Error(), "insufficient funds") {
			ErrorResponse(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			ErrorResponse(w, http.StatusConflict, "withdrawal with this external_reference already exists")
			return
		}
		ErrorResponse(w, http.StatusInternalServerError, "withdrawal processing failed")
		return
	}

	JSONResponse(w, http.StatusCreated, map[string]any{
		"status":             "INITIATED",
		"transaction_id":     txID,
		"external_reference": req.ExternalReference,
		"phone_number":       req.PhoneNumber,
		"amount":             req.Amount,
	})
}
type MpesaB2CCallbackRequest struct {
	ExternalReference string `json:"external_reference"`
	ProviderReceipt   string `json:"provider_receipt"`
	ResultCode        int    `json:"result_code"`
	ResultDesc        string `json:"result_desc"`
	// Optional Safaricom Daraja nested Result envelope support
	Result *struct {
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ConversationID           string `json:"ConversationID"`
		TransactionID            string `json:"TransactionID"`
		ResultCode               int    `json:"ResultCode"`
		ResultDesc               string `json:"ResultDesc"`
	} `json:"Result,omitempty"`
}

func parseB2CCallbackPayload(body []byte, isTimeout bool) (db.B2CCallbackParams, error) {
	var raw MpesaB2CCallbackRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return db.B2CCallbackParams{}, err
	}

	params := db.B2CCallbackParams{
		ExternalReference: strings.TrimSpace(raw.ExternalReference),
		ProviderReceipt:   strings.TrimSpace(raw.ProviderReceipt),
		ResultCode:        raw.ResultCode,
		ResultDesc:        strings.TrimSpace(raw.ResultDesc),
		IsTimeout:         isTimeout,
	}

	if raw.Result != nil {
		if params.ExternalReference == "" {
			params.ExternalReference = strings.TrimSpace(raw.Result.OriginatorConversationID)
		}
		if params.ExternalReference == "" {
			params.ExternalReference = strings.TrimSpace(raw.Result.ConversationID)
		}
		if params.ProviderReceipt == "" {
			params.ProviderReceipt = strings.TrimSpace(raw.Result.TransactionID)
		}
		params.ResultCode = raw.Result.ResultCode
		if params.ResultDesc == "" {
			params.ResultDesc = strings.TrimSpace(raw.Result.ResultDesc)
		}
	}

	if isTimeout && params.ResultCode == 0 {
		params.ResultCode = 1037 // Standard Daraja timeout code
	}
	if params.ResultDesc == "" {
		if isTimeout {
			params.ResultDesc = "B2C queue timeout from provider"
		} else if params.ResultCode == 0 {
			params.ResultDesc = "The service request is processed successfully."
		} else {
			params.ResultDesc = "B2C payout rejected by provider"
		}
	}

	return params, nil
}

// HandleMpesaB2CResultWebhook processes HMAC-signed B2C payout confirmation or failure callbacks.
func (s *Server) HandleMpesaB2CResultWebhook(w http.ResponseWriter, r *http.Request) {
	s.processB2CCallback(w, r, false)
}

// HandleMpesaB2CTimeoutWebhook processes HMAC-signed B2C queue timeout callbacks and triggers auto-reversal.
func (s *Server) HandleMpesaB2CTimeoutWebhook(w http.ResponseWriter, r *http.Request) {
	s.processB2CCallback(w, r, true)
}

func (s *Server) processB2CCallback(w http.ResponseWriter, r *http.Request, isTimeout bool) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, "failed to read callback body")
		return
	}
	defer r.Body.Close()

	secret := os.Getenv("WEBHOOK_SECRET")
	if secret != "" {
		ts := r.Header.Get("X-Pganji-Timestamp")
		sig := r.Header.Get("X-Pganji-Signature")
		if !outbox.VerifyWebhookSignature(secret, ts, sig, bodyBytes, 5*time.Minute) {
			ErrorResponse(w, http.StatusUnauthorized, "invalid or expired webhook signature")
			return
		}
	}

	params, err := parseB2CCallbackPayload(bodyBytes, isTimeout)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if params.ExternalReference == "" {
		ErrorResponse(w, http.StatusBadRequest, "external_reference (or OriginatorConversationID) is required")
		return
	}

	outcome, err := db.SettleB2CResultTx(r.Context(), s.Pool, params)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not found"):
			ErrorResponse(w, http.StatusNotFound, msg)
		case strings.Contains(msg, "already finalized"):
			ErrorResponse(w, http.StatusConflict, msg)
		case strings.Contains(msg, "not a B2C withdrawal"):
			ErrorResponse(w, http.StatusBadRequest, msg)
		default:
			ErrorResponse(w, http.StatusInternalServerError, "failed to process B2C callback")
		}
		return
	}

	JSONResponse(w, http.StatusOK, outcome)
}
