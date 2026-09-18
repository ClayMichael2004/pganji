package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type StatementEntry struct {
	EntryID       string    `json:"entry_id"`
	TransactionID string    `json:"transaction_id"`
	Reference     string    `json:"reference"`
	Description   string    `json:"description"`
	EntryType     string    `json:"entry_type"`
	Amount        int64     `json:"amount"`
	CreatedAt     time.Time `json:"created_at"`
}

type PaginatedStatementResponse struct {
	AccountID  string           `json:"account_id"`
	Entries    []StatementEntry `json:"entries"`
	NextCursor string           `json:"next_cursor,omitempty"`
	HasMore    bool             `json:"has_more"`
}

func encodeCursor(t time.Time, id string) string {
	raw := fmt.Sprintf("%s|%s", t.Format(time.RFC3339Nano), id)
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	bytes, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.Split(string(bytes), "|")
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("invalid cursor format")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return t, parts[1], err
}

func (s *Server) HandleGetAccountStatement(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "account_id parameter is required"})
		return
	}

	// 1. Authorization check: Ensure user owns this account
	authUserID, ok := r.Context().Value(UserContextKey).(string)
	if !ok {
		JSONResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var exists bool
	ownerCheckQuery := `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1 AND user_id = $2);`
	if err := s.Pool.QueryRow(r.Context(), ownerCheckQuery, accountID, authUserID).Scan(&exists); err != nil || !exists {
		JSONResponse(w, http.StatusForbidden, map[string]string{"error": "you do not have permission to view this account"})
		return
	}

	// 2. Parse Dynamic Query Params
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 && parsedLimit <= 100 {
		limit = parsedLimit
	}

	cursorStr := r.URL.Query().Get("cursor")
	var (
		rowsErr error
		query   string
		args    []any
	)

	// Fetch limit + 1 to detect if there is a next page
	fetchLimit := limit + 1

	if cursorStr != "" {
		cursorTime, cursorID, err := decodeCursor(cursorStr)
		if err != nil {
			JSONResponse(w, http.StatusBadRequest, map[string]string{"error": "malformed cursor"})
			return
		}
		query = `
			SELECT 
				j.id, j.transaction_id, t.reference, t.description, j.entry_type, j.amount, j.created_at
			FROM journal_entries j
			JOIN transactions t ON t.id = j.transaction_id
			WHERE j.account_id = $1 
			  AND (j.created_at, j.id) < ($2, $3)
			ORDER BY j.created_at DESC, j.id DESC
			LIMIT $4;
		`
		args = []any{accountID, cursorTime, cursorID, fetchLimit}
	} else {
		query = `
			SELECT 
				j.id, j.transaction_id, t.reference, t.description, j.entry_type, j.amount, j.created_at
			FROM journal_entries j
			JOIN transactions t ON t.id = j.transaction_id
			WHERE j.account_id = $1
			ORDER BY j.created_at DESC, j.id DESC
			LIMIT $2;
		`
		args = []any{accountID, fetchLimit}
	}

	rows, rowsErr := s.Pool.Query(r.Context(), query, args...)
	if rowsErr != nil {
		JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to query ledger entries"})
		return
	}
	defer rows.Close()

	var entries []StatementEntry
	for rows.Next() {
		var entry StatementEntry
		if err := rows.Scan(
			&entry.EntryID,
			&entry.TransactionID,
			&entry.Reference,
			&entry.Description,
			&entry.EntryType,
			&entry.Amount,
			&entry.CreatedAt,
		); err != nil {
			JSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan row"})
			return
		}
		entries = append(entries, entry)
	}

	hasMore := false
	nextCursor := ""

	if len(entries) > limit {
		hasMore = true
		entries = entries[:limit] // trim to requested limit
		lastEntry := entries[len(entries)-1]
		nextCursor = encodeCursor(lastEntry.CreatedAt, lastEntry.EntryID)
	}

	JSONResponse(w, http.StatusOK, PaginatedStatementResponse{
		AccountID:  accountID,
		Entries:    entries,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	})
}