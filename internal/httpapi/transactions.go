package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
	"github.com/EmersonAraki/go-ledger/internal/idempotency"
	"github.com/EmersonAraki/go-ledger/internal/ledger"
)

// createTransactionRequest is the POST /transactions body.
//
// Direction follows accounting rather than banking: the money ends up in
// debit_account_id, funded by credit_account_id.
type createTransactionRequest struct {
	DebitAccountID  string  `json:"debit_account_id"`
	CreditAccountID string  `json:"credit_account_id"`
	Amount          int64   `json:"amount"`
	Currency        string  `json:"currency"`
	Description     string  `json:"description"`
	ExternalRef     *string `json:"external_ref"`
}

type entryResponse struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Direction string    `json:"direction"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
}

type transactionResponse struct {
	ID          uuid.UUID       `json:"id"`
	ExternalRef *string         `json:"external_ref"`
	Description string          `json:"description"`
	Currency    string          `json:"currency"`
	CreatedAt   string          `json:"created_at"`
	Entries     []entryResponse `json:"entries"`
}

func newTransactionResponse(t *ledger.Transaction) transactionResponse {
	entries := make([]entryResponse, 0, len(t.Entries))
	for _, e := range t.Entries {
		entries = append(entries, entryResponse{
			ID:        e.ID,
			AccountID: e.AccountID,
			Direction: string(e.Direction),
			Amount:    e.Amount,
			Currency:  e.Currency,
		})
	}
	return transactionResponse{
		ID:          t.ID,
		ExternalRef: t.ExternalRef,
		Description: t.Description,
		Currency:    t.Currency,
		CreatedAt:   t.CreatedAt.UTC().Format(timeFormat),
		Entries:     entries,
	}
}

// transactionsEndpoint scopes idempotency keys, so the same key used on a
// different endpoint does not collide with this one.
const transactionsEndpoint = "POST /transactions"

// handleCreateTransaction posts a two-leg transfer, exactly once per
// Idempotency-Key.
//
// The key is mandatory. A transfer that a client cannot safely retry is a
// transfer that either goes missing or happens twice when the network fails,
// and neither is acceptable for money.
func (s *Server) handleCreateTransaction(w http.ResponseWriter, r *http.Request) {
	rawKey := r.Header.Get(idempotency.HeaderKey)
	if err := idempotency.ValidateKey(rawKey); err != nil {
		writeDomainError(w, err)
		return
	}

	// Read the body once: the fingerprint must hash exactly what the client
	// sent, and decoding would consume the reader.
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}

	var req createTransactionRequest
	if !unmarshalJSON(w, body, &req) {
		return
	}

	debitID, ok := parseUUIDField(w, "debit_account_id", req.DebitAccountID)
	if !ok {
		return
	}
	creditID, ok := parseUUIDField(w, "credit_account_id", req.CreditAccountID)
	if !ok {
		return
	}

	fingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)
	if err != nil {
		problem.Write(w, http.StatusBadRequest, "invalid_json", "Invalid JSON", err.Error())
		return
	}

	claim := ledger.Claim{
		Key:         rawKey,
		Endpoint:    transactionsEndpoint,
		Fingerprint: fingerprint,
	}

	// Rendering happens inside the database transaction that creates the
	// transfer, because these exact bytes are stored as the reply a later
	// duplicate will receive.
	render := func(t *ledger.Transaction) (int, []byte, error) {
		encoded, err := json.Marshal(newTransactionResponse(t))
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, encoded, nil
	}

	result, err := s.ledger.Transfer(r.Context(), ledger.TransferCommand{
		DebitAccountID:  debitID,
		CreditAccountID: creditID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Description:     req.Description,
		ExternalRef:     req.ExternalRef,
	}, claim, render)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	if result.Replayed {
		w.Header().Set(idempotency.HeaderReplayed, "true")
	}
	// The stored body is written back verbatim, so a replay is byte-identical
	// to the response the first request received.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.Status)
	if _, err := w.Write(result.Body); err != nil {
		slog.ErrorContext(r.Context(), "write transaction response", "error", err)
	}
}

func (s *Server) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	tx, err := s.ledger.GetTransaction(r.Context(), id)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, newTransactionResponse(tx))
}
