package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
	"github.com/EmersonAraki/go-ledger/internal/ledger"
)

// createAccountRequest is the POST /accounts body.
type createAccountRequest struct {
	Name                 string `json:"name"`
	Kind                 string `json:"kind"`
	Currency             string `json:"currency"`
	AllowNegativeBalance bool   `json:"allow_negative_balance"`
}

// accountResponse is how an account appears on the wire. Balance is in minor
// units alongside its currency; never a decimal string or a float.
type accountResponse struct {
	ID                   uuid.UUID `json:"id"`
	Name                 string    `json:"name"`
	Kind                 string    `json:"kind"`
	Currency             string    `json:"currency"`
	Balance              int64     `json:"balance"`
	AllowNegativeBalance bool      `json:"allow_negative_balance"`
	CreatedAt            string    `json:"created_at"`
	UpdatedAt            string    `json:"updated_at"`
}

func newAccountResponse(a *ledger.Account) accountResponse {
	return accountResponse{
		ID:                   a.ID,
		Name:                 a.Name,
		Kind:                 string(a.Kind),
		Currency:             a.Currency,
		Balance:              a.Balance,
		AllowNegativeBalance: a.AllowNegativeBalance,
		CreatedAt:            a.CreatedAt.UTC().Format(timeFormat),
		UpdatedAt:            a.UpdatedAt.UTC().Format(timeFormat),
	}
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	account, err := s.ledger.CreateAccount(r.Context(), ledger.CreateAccountCommand{
		Name:                 req.Name,
		Kind:                 ledger.AccountKind(req.Kind),
		Currency:             req.Currency,
		AllowNegativeBalance: req.AllowNegativeBalance,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, newAccountResponse(account))
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	account, err := s.ledger.GetAccount(r.Context(), id)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, newAccountResponse(account))
}

// parseUUIDParam reads a UUID path parameter, writing a 400 and returning false
// when it is not a UUID. A malformed id is a client error, not a 404.
func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		problem.Write(w, http.StatusBadRequest, "invalid_uuid", "Invalid UUID",
			"path parameter "+name+" is not a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}
