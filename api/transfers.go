package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"pganji/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct{
	Pool *pgxpool.Pool
}

type CreateTransferRequest struct{
	FromAccountID 	string `json:"from_account_id"`
	ToAccountID 	string `json:"to_account_id"`
	Amount 			int64 `json:"amount"`
	Reference 		string `json:"reference"`
	Description 	string `json:"description"`
}

func (s *Server) HandleTransfer(w http.ResponseWriter, r *http.Request){
	var req CreateTransferRequest
	if err:=json.NewEncoder(r.Body).Decode(&req); err!=nil{
		ErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	//strict input validation
	if strings.TrimSpace(req.FromAccountID)=="" || strings.TrimSpace(req.ToAccountID)==""{
		ErrorResponse(w, http.StatusBadRequest, "from_account_id and to_account_id are required")
		return
	}

	if req.FromAccountID==req.ToAccountID{
		ErrorResponse(w, http.StatusBadRequest, "cannot transfer funds to the same account")
		return
	}

	if req.Amount<=0{
		ErrorResponse(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}

	if strings.TrimSpace(req.Reference)==""{
		ErrorResponse(w, http.StatusBadRequest, "reference is required")
		return
	}

	//execute transfer via core ledger engine
	params:= db.TransferParams{
		FromAccountID: req.FromAccountID,
		ToAccountID: req.ToAccountID,
		Amount: req.Amount,
		Reference: req.Reference,
		Description: req.Description,
	}

	err:= db.TransferTx(r.Context(), s.Pool, params)
	if err !=nil{
		//differentiate business domain/insuffecient funds errors from internal server errors
		if strings.Contains(err.Error(), "insufficient funds"){
			ErrorResponse(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique"){
			ErrorResponse(w, http.StatusConflict, "transaction with this reference already exists")
			return
		}
		ErrorResponse(w, http.StatusInternalServerError, "transfer processing failed")
		return
	}

	//return 201 created success
	JSONResponse(w, http.StatusCreated, map[string]any{
		"status": "COMPLETED",
		"message": "Transfer executed successfuly"
		"reference": req.Reference,
		"amount": req.Amount,
	})
}

func(s *Server) HandleGetBalance(w http.ResponseWriter, r *http.Request){
	//query param /api/v1/accounts/balance?account_id=<UUID>
	accountID:= r.URl.Query().get("account_id")
	if strings.Trimspace(accountID)==""{
		ErrorResponse(w, http.StatusBadRequest, "account_id query parameter is required")
		return
	}
	tx, err:= s.Pool.Begin(r.Context())
	if err!=nil{
		ErrorResponse(w, http.StatusInternalServerError, "faileds to start read transaction")
		return
	}
	defer tx.Rollback(r.Context())

	balance, err:= db.GetAccountBalance(r.Context(), tx, accountID)
	if err !=nil{
		ErrorResponse(w, http.StatusInternalServerError, "Failed to fetch balance")
		return
	}

	JSONResponse(w, http.StatusOK, map[string]any{
		"account_id": accountID,
		"balance": balance,
		"formatted": float64(balance)/100.0
	})
}