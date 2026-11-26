package api

import (
	"net/http"

	"pganji/db"
)

func (s *Server) HandleVerifyLedgerChain(w http.ResponseWriter, r *http.Request) {
	report, err := db.VerifyLedgerHashChain(r.Context(), s.Pool)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "failed to execute cryptographic ledger audit")
		return
	}

	statusCode := http.StatusOK
	if report.Status != "VALID" {
		statusCode = http.StatusConflict
	}

	JSONResponse(w, statusCode, report)
}
