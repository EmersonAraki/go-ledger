package httpapi

import (
	"errors"
	"net/http"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
	"github.com/EmersonAraki/go-ledger/internal/ledger"
)

// errorMapping ties one domain error to the wire representation clients see.
type errorMapping struct {
	status int
	typ    string
	title  string
}

// domainErrors is the single place where domain meaning becomes an HTTP status.
// Handlers never construct error responses themselves, so the mapping cannot
// drift between endpoints.
//
// The 400/422 split follows RFC 4918: 400 for a request that is malformed or
// violates a rule visible in the request itself, 422 for one that is
// well-formed but conflicts with the state of the ledger.
var domainErrors = []struct {
	err error
	errorMapping
}{
	{ledger.ErrAccountNotFound, errorMapping{http.StatusNotFound, "account_not_found", "Account Not Found"}},
	{ledger.ErrTransactionNotFound, errorMapping{http.StatusNotFound, "transaction_not_found", "Transaction Not Found"}},
	{ledger.ErrInsufficientFunds, errorMapping{http.StatusUnprocessableEntity, "insufficient_funds", "Insufficient Funds"}},
	{ledger.ErrCurrencyMismatch, errorMapping{http.StatusUnprocessableEntity, "currency_mismatch", "Currency Mismatch"}},
	{ledger.ErrDuplicateExternalRef, errorMapping{http.StatusConflict, "duplicate_external_ref", "Duplicate External Reference"}},
	{ledger.ErrSameAccount, errorMapping{http.StatusBadRequest, "same_account", "Same Account"}},
	{ledger.ErrInvalidAmount, errorMapping{http.StatusBadRequest, "invalid_amount", "Invalid Amount"}},
	{ledger.ErrInvalidCurrency, errorMapping{http.StatusBadRequest, "invalid_currency", "Invalid Currency"}},
	{ledger.ErrInvalidAccountKind, errorMapping{http.StatusBadRequest, "invalid_account_kind", "Invalid Account Kind"}},
	{ledger.ErrEmptyName, errorMapping{http.StatusBadRequest, "invalid_name", "Invalid Name"}},
}

// writeDomainError renders err as problem+json. Anything unrecognised is an
// internal error: it is logged in full and reported to the client without
// detail, so an unexpected failure never leaks database internals.
func writeDomainError(w http.ResponseWriter, err error) {
	for _, m := range domainErrors {
		if errors.Is(err, m.err) {
			problem.Write(w, m.status, m.typ, m.title, err.Error())
			return
		}
	}
	problem.Internal(w, err)
}
