package httpapi_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/idempotency"
)

// transferBody builds a valid transfer payload.
func transferBody(debit, credit string, amount int64) map[string]any {
	return map[string]any{
		"debit_account_id":  debit,
		"credit_account_id": credit,
		"amount":            amount,
		"currency":          "BRL",
	}
}

func TestMissingIdempotencyKeyIsRejected(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	rec := api.doWithKey(http.MethodPost, "/transactions", transferBody(bob, alice, 100), "")
	api.assertProblem(rec, http.StatusBadRequest, "idempotency_key_required")

	// A rejected request must not have moved anything.
	if got := api.balanceOf(alice); got != 1000 {
		t.Errorf("alice balance = %d, want 1000", got)
	}
}

func TestMalformedIdempotencyKeyIsRejected(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	long := make([]byte, idempotency.MaxKeyLength+1)
	for i := range long {
		long[i] = 'k'
	}

	for name, key := range map[string]string{
		"too long":          string(long),
		"control character": "abc\x01def",
	} {
		t.Run(name, func(t *testing.T) {
			rec := api.doWithKey(http.MethodPost, "/transactions", transferBody(bob, alice, 100), key)
			api.assertProblem(rec, http.StatusBadRequest, "idempotency_key_invalid")
		})
	}
}

// A retry under the same key must return the first response and do no new work.
func TestRetryWithSameKeyReplaysTheStoredResponse(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	key := uuid.NewString()
	body := transferBody(bob, alice, 300)

	first := api.doWithKey(http.MethodPost, "/transactions", body, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first request: status %d, body %s", first.Code, first.Body)
	}
	if first.Header().Get(idempotency.HeaderReplayed) != "" {
		t.Error("first request must not be marked as a replay")
	}

	second := api.doWithKey(http.MethodPost, "/transactions", body, key)
	if second.Code != first.Code {
		t.Errorf("replay status = %d, want %d", second.Code, first.Code)
	}
	if second.Header().Get(idempotency.HeaderReplayed) != "true" {
		t.Error("replay must set the Idempotency-Replayed header")
	}
	// Byte-identical, not merely equivalent: the client should not be able to
	// tell the replay apart from the original except by the header.
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay body differs:\n first: %s\nsecond: %s", first.Body, second.Body)
	}

	// And the money moved exactly once.
	if got := api.balanceOf(bob); got != 300 {
		t.Errorf("bob balance = %d, want 300 -- the retry moved money twice", got)
	}
	if got := api.balanceOf(alice); got != 700 {
		t.Errorf("alice balance = %d, want 700", got)
	}
	assertLedgerSumsToZero(t, api)
}

// Field order and whitespace do not change what a request means, so they must
// not change its fingerprint.
func TestReplayIgnoresFieldOrderAndWhitespace(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	key := uuid.NewString()

	first := api.doWithKey(http.MethodPost, "/transactions",
		map[string]any{
			"debit_account_id":  bob,
			"credit_account_id": alice,
			"amount":            300,
			"currency":          "BRL",
		}, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first request: status %d, body %s", first.Code, first.Body)
	}

	// Same fields, different declaration order.
	second := api.doWithKey(http.MethodPost, "/transactions",
		map[string]any{
			"currency":          "BRL",
			"amount":            300,
			"credit_account_id": alice,
			"debit_account_id":  bob,
		}, key)

	if second.Header().Get(idempotency.HeaderReplayed) != "true" {
		t.Errorf("reordered but identical request should replay, got status %d body %s",
			second.Code, second.Body)
	}
	if got := api.balanceOf(bob); got != 300 {
		t.Errorf("bob balance = %d, want 300", got)
	}
}

// Reusing a key for a genuinely different request is a client bug. Returning
// the first response would be wrong, and executing the new one would break the
// guarantee, so it is refused.
func TestSameKeyDifferentPayloadIsRefused(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	key := uuid.NewString()

	if rec := api.doWithKey(http.MethodPost, "/transactions",
		transferBody(bob, alice, 300), key); rec.Code != http.StatusCreated {
		t.Fatalf("first request: status %d, body %s", rec.Code, rec.Body)
	}

	// Same key, different amount.
	rec := api.doWithKey(http.MethodPost, "/transactions", transferBody(bob, alice, 999), key)
	api.assertProblem(rec, http.StatusUnprocessableEntity, "idempotency_key_reuse")

	if got := api.balanceOf(bob); got != 300 {
		t.Errorf("bob balance = %d, want 300 -- the refused request moved money", got)
	}
	assertLedgerSumsToZero(t, api)
}

// A request that fails must not burn its key: the client should be able to fix
// the payload and retry with the same one.
func TestFailedRequestLeavesTheKeyUsable(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 100)

	key := uuid.NewString()

	// Fails on insufficient funds, after the key would have been claimed.
	rec := api.doWithKey(http.MethodPost, "/transactions", transferBody(bob, alice, 5000), key)
	api.assertProblem(rec, http.StatusUnprocessableEntity, "insufficient_funds")

	// The same key now works for a corrected request.
	rec = api.doWithKey(http.MethodPost, "/transactions", transferBody(bob, alice, 50), key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after a failure should succeed, got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get(idempotency.HeaderReplayed) != "" {
		t.Error("the corrected request did real work; it must not be marked a replay")
	}

	if got := api.balanceOf(bob); got != 50 {
		t.Errorf("bob balance = %d, want 50", got)
	}
	assertLedgerSumsToZero(t, api)
}

// The flagship test: many identical requests racing on one key.
//
// This is what the whole single-transaction claim protocol exists for. A
// duplicate that arrives while the original is still in flight blocks on the
// claim row until it commits or aborts, so it can never observe a half-finished
// transfer.
func TestConcurrentRequestsWithSameKeyTransferExactlyOnce(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	const concurrency = 30
	key := uuid.NewString()
	body := transferBody(bob, alice, 300)

	type outcome struct {
		status   int
		replayed bool
		body     string
	}
	results := make([]outcome, concurrency)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range concurrency {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release everyone at once
			rec := api.doWithKey(http.MethodPost, "/transactions", body, key)
			results[i] = outcome{
				status:   rec.Code,
				replayed: rec.Header().Get(idempotency.HeaderReplayed) == "true",
				body:     rec.Body.String(),
			}
		}()
	}
	start.Done()
	done.Wait()

	var executed, replayed int
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Errorf("request %d: status %d, want 201 (body %s)", i, r.status, r.body)
			continue
		}
		if r.replayed {
			replayed++
		} else {
			executed++
		}
		// Every caller must receive the same answer.
		if r.body != results[0].body {
			t.Errorf("request %d returned a different body than request 0", i)
		}
	}

	if executed != 1 {
		t.Errorf("%d requests did real work, want exactly 1", executed)
	}
	if replayed != concurrency-1 {
		t.Errorf("%d requests replayed, want %d", replayed, concurrency-1)
	}

	// The decisive assertion: the money moved once, no matter how many callers.
	if got := api.balanceOf(bob); got != 300 {
		t.Errorf("bob balance = %d, want 300 after %d concurrent identical requests",
			got, concurrency)
	}
	if got := api.balanceOf(alice); got != 700 {
		t.Errorf("alice balance = %d, want 700", got)
	}

	var entries int
	if err := api.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM ledger_entries WHERE amount = 300`).Scan(&entries); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entries != 2 {
		t.Errorf("got %d ledger entries for the transfer, want exactly 2", entries)
	}

	assertLedgerSumsToZero(t, api)
}
