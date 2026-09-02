package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/httpapi"
	"github.com/EmersonAraki/go-ledger/internal/idempotency"
	"github.com/EmersonAraki/go-ledger/internal/ledger"
	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// testAPI is a live handler backed by a real, isolated database schema.
type testAPI struct {
	t       *testing.T
	handler http.Handler
	pool    *pgxpool.Pool
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := pgtest.Pool(t)
	svc := ledger.NewService(postgres.NewStore(pool))
	return &testAPI{t: t, handler: httpapi.NewServer(pool, svc).Routes(), pool: pool}
}

// do issues a request with a fresh idempotency key, so ordinary tests never
// collide with one another.
func (a *testAPI) do(method, path string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.doWithKey(method, path, body, uuid.NewString())
}

// doWithKey issues a request under a caller-chosen idempotency key.
func (a *testAPI) doWithKey(method, path string, body any, key string) *httptest.ResponseRecorder {
	a.t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			a.t.Fatalf("encode request: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}

	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

// createAccount posts an account and returns its id, failing the test if the
// call does not succeed.
func (a *testAPI) createAccount(name, currency string, allowNegative bool) string {
	a.t.Helper()

	rec := a.do(http.MethodPost, "/accounts", map[string]any{
		"name":                   name,
		"kind":                   "asset",
		"currency":               currency,
		"allow_negative_balance": allowNegative,
	})
	if rec.Code != http.StatusCreated {
		a.t.Fatalf("create account %s: status %d, body %s", name, rec.Code, rec.Body)
	}

	var resp struct {
		ID string `json:"id"`
	}
	a.decode(rec, &resp)
	return resp.ID
}

// fund gives an account a starting balance by moving money in from a system
// account that is allowed to go negative -- the only way money enters a ledger
// that must sum to zero.
func (a *testAPI) fund(accountID, currency string, amount int64) {
	a.t.Helper()

	source := a.createAccount("external-"+accountID[:8], currency, true)
	rec := a.do(http.MethodPost, "/transactions", map[string]any{
		"debit_account_id":  accountID,
		"credit_account_id": source,
		"amount":            amount,
		"currency":          currency,
	})
	if rec.Code != http.StatusCreated {
		a.t.Fatalf("fund account: status %d, body %s", rec.Code, rec.Body)
	}
}

func (a *testAPI) decode(rec *httptest.ResponseRecorder, dst any) {
	a.t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		a.t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// balanceOf reads an account's current balance through the API.
func (a *testAPI) balanceOf(id string) int64 {
	a.t.Helper()

	rec := a.do(http.MethodGet, "/accounts/"+id, nil)
	if rec.Code != http.StatusOK {
		a.t.Fatalf("get account: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Balance int64 `json:"balance"`
	}
	a.decode(rec, &resp)
	return resp.Balance
}

// assertProblem checks the response is problem+json with the expected status
// and machine-readable type.
func (a *testAPI) assertProblem(rec *httptest.ResponseRecorder, status int, typ string) {
	a.t.Helper()

	if rec.Code != status {
		a.t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		a.t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p struct {
		Type string `json:"type"`
	}
	a.decode(rec, &p)
	if p.Type != typ {
		a.t.Errorf("problem type = %q, want %q", p.Type, typ)
	}
}

func TestCreateAndGetAccount(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	id := api.createAccount("alice", "BRL", false)

	rec := api.do(http.MethodGet, "/accounts/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Currency string `json:"currency"`
		Balance  int64  `json:"balance"`
	}
	api.decode(rec, &resp)

	if resp.ID != id || resp.Name != "alice" || resp.Kind != "asset" || resp.Currency != "BRL" {
		t.Errorf("unexpected account: %+v", resp)
	}
	// A new account starts empty: money only ever arrives via a transaction, so
	// the ledger sums to zero from the first write onwards.
	if resp.Balance != 0 {
		t.Errorf("new account balance = %d, want 0", resp.Balance)
	}
}

func TestCreateAccountRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	cases := []struct {
		name string
		body map[string]any
		typ  string
	}{
		{"empty name", map[string]any{"name": "", "kind": "asset", "currency": "BRL"}, "invalid_name"},
		{"unknown kind", map[string]any{"name": "a", "kind": "wallet", "currency": "BRL"}, "invalid_account_kind"},
		{"lowercase currency", map[string]any{"name": "a", "kind": "asset", "currency": "brl"}, "invalid_currency"},
		{"short currency", map[string]any{"name": "a", "kind": "asset", "currency": "BR"}, "invalid_currency"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := api.do(http.MethodPost, "/accounts", tc.body)
			api.assertProblem(rec, http.StatusBadRequest, tc.typ)
		})
	}
}

func TestGetUnknownAccountReturns404(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	rec := api.do(http.MethodGet, "/accounts/2a1c4f6e-0000-4000-8000-000000000000", nil)
	api.assertProblem(rec, http.StatusNotFound, "account_not_found")
}

func TestMalformedUUIDIsBadRequestNotNotFound(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	rec := api.do(http.MethodGet, "/accounts/not-a-uuid", nil)
	api.assertProblem(rec, http.StatusBadRequest, "invalid_uuid")
}

func TestTransferMovesMoneyAndBalancesTheLedger(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	rec := api.do(http.MethodPost, "/transactions", map[string]any{
		"debit_account_id":  bob,
		"credit_account_id": alice,
		"amount":            300,
		"currency":          "BRL",
		"description":       "rent",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	var tx struct {
		ID      string `json:"id"`
		Entries []struct {
			AccountID string `json:"account_id"`
			Direction string `json:"direction"`
			Amount    int64  `json:"amount"`
		} `json:"entries"`
	}
	api.decode(rec, &tx)

	if len(tx.Entries) != 2 {
		t.Fatalf("got %d entries, want exactly 2", len(tx.Entries))
	}

	// Money moved from the credited account to the debited one.
	if got := api.balanceOf(bob); got != 300 {
		t.Errorf("bob balance = %d, want 300", got)
	}
	if got := api.balanceOf(alice); got != 700 {
		t.Errorf("alice balance = %d, want 700", got)
	}

	// The invariant the whole system exists to hold.
	assertLedgerSumsToZero(t, api)

	// And the transaction reads back with both legs.
	rec = api.do(http.MethodGet, "/transactions/"+tx.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get transaction: status %d", rec.Code)
	}
	var fetched struct {
		Entries []struct {
			Direction string `json:"direction"`
		} `json:"entries"`
	}
	api.decode(rec, &fetched)
	if len(fetched.Entries) != 2 {
		t.Errorf("fetched %d entries, want 2", len(fetched.Entries))
	}
}

func TestTransferRejectsUnknownAccount(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)

	rec := api.do(http.MethodPost, "/transactions", map[string]any{
		"debit_account_id":  "2a1c4f6e-0000-4000-8000-000000000000",
		"credit_account_id": alice,
		"amount":            100,
		"currency":          "BRL",
	})
	api.assertProblem(rec, http.StatusNotFound, "account_not_found")
	assertLedgerSumsToZero(t, api)
}

func TestTransferRejectsCurrencyMismatch(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	brl := api.createAccount("brl-account", "BRL", false)
	usd := api.createAccount("usd-account", "USD", false)
	api.fund(brl, "BRL", 1000)

	rec := api.do(http.MethodPost, "/transactions", map[string]any{
		"debit_account_id":  usd,
		"credit_account_id": brl,
		"amount":            100,
		"currency":          "BRL",
	})
	api.assertProblem(rec, http.StatusUnprocessableEntity, "currency_mismatch")

	// A rejected transfer must leave both balances untouched.
	if got := api.balanceOf(brl); got != 1000 {
		t.Errorf("brl balance = %d, want 1000 after rejected transfer", got)
	}
	if got := api.balanceOf(usd); got != 0 {
		t.Errorf("usd balance = %d, want 0 after rejected transfer", got)
	}
	assertLedgerSumsToZero(t, api)
}

func TestTransferRejectsInsufficientFunds(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 100)

	rec := api.do(http.MethodPost, "/transactions", map[string]any{
		"debit_account_id":  bob,
		"credit_account_id": alice,
		"amount":            500,
		"currency":          "BRL",
	})
	api.assertProblem(rec, http.StatusUnprocessableEntity, "insufficient_funds")

	if got := api.balanceOf(alice); got != 100 {
		t.Errorf("alice balance = %d, want 100 (unchanged)", got)
	}
	if got := api.balanceOf(bob); got != 0 {
		t.Errorf("bob balance = %d, want 0 (unchanged)", got)
	}
	assertLedgerSumsToZero(t, api)
}

func TestTransferRejectsNonPositiveAndSelfTransfer(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	cases := []struct {
		name   string
		debit  string
		credit string
		amount int64
		typ    string
	}{
		{"zero amount", bob, alice, 0, "invalid_amount"},
		{"negative amount", bob, alice, -100, "invalid_amount"},
		{"same account", alice, alice, 100, "same_account"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := api.do(http.MethodPost, "/transactions", map[string]any{
				"debit_account_id":  tc.debit,
				"credit_account_id": tc.credit,
				"amount":            tc.amount,
				"currency":          "BRL",
			})
			api.assertProblem(rec, http.StatusBadRequest, tc.typ)
		})
	}

	if got := api.balanceOf(alice); got != 1000 {
		t.Errorf("alice balance = %d, want 1000 (unchanged)", got)
	}
	assertLedgerSumsToZero(t, api)
}

func TestDuplicateExternalRefIsConflict(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	body := map[string]any{
		"debit_account_id":  bob,
		"credit_account_id": alice,
		"amount":            100,
		"currency":          "BRL",
		"external_ref":      "TRX-1",
	}

	if rec := api.do(http.MethodPost, "/transactions", body); rec.Code != http.StatusCreated {
		t.Fatalf("first post: status %d, body %s", rec.Code, rec.Body)
	}

	rec := api.do(http.MethodPost, "/transactions", body)
	api.assertProblem(rec, http.StatusConflict, "duplicate_external_ref")

	// The rejected retry must not have moved money a second time.
	if got := api.balanceOf(bob); got != 100 {
		t.Errorf("bob balance = %d, want 100", got)
	}
	assertLedgerSumsToZero(t, api)
}

func TestMalformedBodyIsRejected(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	// An unknown field usually means a typo; silently ignoring it could post a
	// transfer of zero.
	rec := api.do(http.MethodPost, "/accounts", map[string]any{
		"name": "a", "kind": "asset", "currency": "BRL", "balnce": 500,
	})
	api.assertProblem(rec, http.StatusBadRequest, "invalid_json")

	req := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString("{not json"))
	rec = httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)
	api.assertProblem(rec, http.StatusBadRequest, "invalid_json")
}

// assertLedgerSumsToZero is the global invariant: across every entry ever
// written, debits and credits cancel exactly.
func assertLedgerSumsToZero(t *testing.T, api *testAPI) {
	t.Helper()

	var sum int64
	if err := api.pool.QueryRow(t.Context(),
		`SELECT COALESCE(SUM(signed_amount), 0) FROM ledger_entries`).Scan(&sum); err != nil {
		t.Fatalf("sum ledger entries: %v", err)
	}
	if sum != 0 {
		t.Errorf("ledger does not sum to zero: %d", sum)
	}
}
