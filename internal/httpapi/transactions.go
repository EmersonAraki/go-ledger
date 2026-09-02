package httpapi

import (
	"net/http"

	"github.com/google/uuid"

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

// handleCreateTransaction posts a two-leg transfer.
//
// No idempotency yet: this endpoint will require an Idempotency-Key header in
// phase 3. Until then a retried request creates a second transfer.
func (s *Server) handleCreateTransaction(w http.ResponseWriter, r *http.Request) {
	var req createTransactionRequest
	if !decodeJSON(w, r, &req) {
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

	tx, err := s.ledger.Transfer(r.Context(), ledger.TransferCommand{
		DebitAccountID:  debitID,
		CreditAccountID: creditID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Description:     req.Description,
		ExternalRef:     req.ExternalRef,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, newTransactionResponse(tx))
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
