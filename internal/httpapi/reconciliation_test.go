package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

// uploadStatement posts a CSV to the reconciliation endpoint.
func (a *testAPI) uploadStatement(filename, csv string) *httptest.ResponseRecorder {
	a.t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		a.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(csv)); err != nil {
		a.t.Fatalf("write form file: %v", err)
	}
	if err := form.Close(); err != nil {
		a.t.Fatalf("close form: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/reconciliation", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())

	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

type runBody struct {
	ID               string `json:"id"`
	SourceName       string `json:"source_name"`
	StatementRows    int    `json:"statement_rows"`
	MatchedCount     int    `json:"matched_count"`
	DiscrepancyCount int    `json:"discrepancy_count"`
	Clean            bool   `json:"clean"`
	NextCursor       *int64 `json:"next_cursor"`
	Discrepancies    []struct {
		Kind          string         `json:"kind"`
		StatementRef  *string        `json:"statement_ref"`
		TransactionID *string        `json:"transaction_id"`
		Details       map[string]any `json:"details"`
	} `json:"discrepancies"`
}

func (r runBody) kinds() map[string]int {
	out := map[string]int{}
	for _, d := range r.Discrepancies {
		out[d.Kind]++
	}
	return out
}

const csvHeader = "external_ref,posted_at,debit_account_id,credit_account_id,amount,currency\n"

// postedAt reads a transaction's creation time, so the fixture can use dates the
// ledger will actually agree with.
func (a *testAPI) postedAt(transactionID string) time.Time {
	a.t.Helper()
	var at time.Time
	if err := a.pool.QueryRow(context.Background(),
		`SELECT created_at FROM transactions WHERE id = $1`, transactionID).Scan(&at); err != nil {
		a.t.Fatalf("read transaction time: %v", err)
	}
	return at.UTC()
}

// A statement that agrees with the ledger produces a clean run.
func TestReconcileCleanStatement(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 10_000)

	txID := api.postTransferWithRef(bob, alice, 300, "TRX-1")
	when := api.postedAt(txID).Format(time.RFC3339)

	// The funding transfer is in the window too, so the statement must include
	// it or it would correctly be reported as missing.
	fundingID := api.transactionIDForAmount(10_000)
	fundingRef := api.externalRefFor(fundingID)

	csv := csvHeader +
		fmt.Sprintf("%s,%s,%s,%s,%d,BRL\n", "TRX-1", when, bob, alice, 300) +
		fmt.Sprintf("%s,%s,%s,%s,%d,BRL\n", fundingRef, api.postedAt(fundingID).Format(time.RFC3339),
			alice, api.fundingSource, 10_000)

	rec := api.uploadStatement("clean.csv", csv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)

	if run.SourceName != "clean.csv" {
		t.Errorf("source_name = %q", run.SourceName)
	}
	if run.StatementRows != 2 {
		t.Errorf("statement_rows = %d, want 2", run.StatementRows)
	}
	if run.MatchedCount != 2 {
		t.Errorf("matched_count = %d, want 2", run.MatchedCount)
	}
	if !run.Clean || run.DiscrepancyCount != 0 {
		t.Errorf("expected a clean run, got %d discrepancies: %v", run.DiscrepancyCount, run.kinds())
	}
}

// One fixture exercising every discrepancy kind the matcher can produce.
func TestReconcileReportsEveryDiscrepancyKind(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	carol := api.createAccount("carol", "BRL", false)
	api.fund(alice, "BRL", 100_000)

	// In the ledger and on the statement, but with the wrong amount.
	wrongAmount := api.postTransferWithRef(bob, alice, 300, "TRX-AMOUNT")
	// In the ledger and on the statement, but between different accounts.
	wrongAccounts := api.postTransferWithRef(bob, alice, 400, "TRX-ACCOUNT")
	// In the ledger only.
	api.postTransferWithRef(carol, alice, 700, "TRX-LEDGER-ONLY")
	// In the ledger with no reference; the statement row has none either.
	shapeOnly := api.postTransfer(carol, alice, 900)

	at := func(id string) string { return api.postedAt(id).Format(time.RFC3339) }

	csv := csvHeader +
		// amount_mismatch
		fmt.Sprintf("TRX-AMOUNT,%s,%s,%s,999,BRL\n", at(wrongAmount), bob, alice) +
		// account_mismatch
		fmt.Sprintf("TRX-ACCOUNT,%s,%s,%s,400,BRL\n", at(wrongAccounts), carol, alice) +
		// missing_in_ledger
		fmt.Sprintf("TRX-GHOST,%s,%s,%s,555,BRL\n", at(wrongAmount), bob, alice) +
		// duplicate_in_statement
		fmt.Sprintf("TRX-AMOUNT,%s,%s,%s,999,BRL\n", at(wrongAmount), bob, alice) +
		// probable_match (no reference, same shape as shapeOnly)
		fmt.Sprintf(",%s,%s,%s,900,BRL\n", at(shapeOnly), carol, alice) +
		// unparseable_row
		"TRX-BAD,not-a-date," + alice + "," + bob + ",1,BRL\n"

	rec := api.uploadStatement("messy.csv", csv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)
	got := run.kinds()

	for _, want := range []string{
		reconcile.KindAmountMismatch,
		reconcile.KindAccountMismatch,
		reconcile.KindMissingInLedger,
		reconcile.KindMissingInStatement, // the funding transfer and TRX-LEDGER-ONLY
		reconcile.KindDuplicateInStatement,
		reconcile.KindProbableMatch,
		reconcile.KindUnparseableRow,
	} {
		if got[want] == 0 {
			t.Errorf("expected at least one %s, got: %v", want, got)
		}
	}

	if run.Clean {
		t.Error("run reported clean despite discrepancies")
	}
	if run.DiscrepancyCount != len(run.Discrepancies) {
		t.Errorf("discrepancy_count = %d but %d were returned",
			run.DiscrepancyCount, len(run.Discrepancies))
	}

	// The report must be retrievable afterwards, not only in the upload response.
	rec = api.do(http.MethodGet, "/reconciliation/"+run.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run: status %d, body %s", rec.Code, rec.Body)
	}
	var stored runBody
	api.decode(rec, &stored)
	if stored.DiscrepancyCount != run.DiscrepancyCount {
		t.Errorf("stored run has %d discrepancies, want %d",
			stored.DiscrepancyCount, run.DiscrepancyCount)
	}
	if len(stored.Discrepancies) != len(run.Discrepancies) {
		t.Errorf("stored run returned %d findings, want %d",
			len(stored.Discrepancies), len(run.Discrepancies))
	}
}

// Reconciliation reports; it must never change the ledger.
func TestReconcileNeverModifiesTheLedger(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)
	api.postTransfer(bob, alice, 300)

	beforeAlice, beforeBob := api.balanceOf(alice), api.balanceOf(bob)
	var beforeTx, beforeEntries int
	if err := api.pool.QueryRow(context.Background(),
		`SELECT (SELECT COUNT(*) FROM transactions), (SELECT COUNT(*) FROM ledger_entries)`).
		Scan(&beforeTx, &beforeEntries); err != nil {
		t.Fatalf("count before: %v", err)
	}

	// A statement full of disagreements, which a "helpful" implementation might
	// be tempted to correct.
	csv := csvHeader +
		fmt.Sprintf("TRX-GHOST-1,2026-09-01T10:00:00Z,%s,%s,99999,BRL\n", bob, alice) +
		fmt.Sprintf("TRX-GHOST-2,2026-09-01T10:00:00Z,%s,%s,88888,BRL\n", alice, bob)

	if rec := api.uploadStatement("ghosts.csv", csv); rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}

	if got := api.balanceOf(alice); got != beforeAlice {
		t.Errorf("alice balance changed: %d -> %d", beforeAlice, got)
	}
	if got := api.balanceOf(bob); got != beforeBob {
		t.Errorf("bob balance changed: %d -> %d", beforeBob, got)
	}

	var afterTx, afterEntries int
	if err := api.pool.QueryRow(context.Background(),
		`SELECT (SELECT COUNT(*) FROM transactions), (SELECT COUNT(*) FROM ledger_entries)`).
		Scan(&afterTx, &afterEntries); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if afterTx != beforeTx || afterEntries != beforeEntries {
		t.Errorf("reconciliation wrote to the ledger: transactions %d->%d, entries %d->%d",
			beforeTx, afterTx, beforeEntries, afterEntries)
	}
	assertLedgerSumsToZero(t, api)
}

// The drift check is what keeps the cached balance honest.
func TestReconcileDetectsBalanceDrift(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)
	api.postTransfer(bob, alice, 300)

	// Corrupt the cache behind the service's back, exactly as a bug elsewhere
	// would. SUM(signed_amount) is unchanged and remains the truth.
	if _, err := api.pool.Exec(context.Background(),
		`UPDATE accounts SET balance = balance + 42 WHERE id = $1`, bob); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}

	rec := api.uploadStatement("empty.csv", csvHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)

	var drift *struct {
		Kind          string         `json:"kind"`
		StatementRef  *string        `json:"statement_ref"`
		TransactionID *string        `json:"transaction_id"`
		Details       map[string]any `json:"details"`
	}
	for i := range run.Discrepancies {
		if run.Discrepancies[i].Kind == reconcile.KindBalanceDrift {
			drift = &run.Discrepancies[i]
			break
		}
	}
	if drift == nil {
		t.Fatalf("balance drift was not detected; kinds: %v", run.kinds())
	}
	if name, _ := drift.Details["account_name"].(string); name != "bob" {
		t.Errorf("drift reported for %q, want bob", name)
	}
	if diff, _ := drift.Details["difference"].(float64); diff != 42 {
		t.Errorf("difference = %v, want 42", diff)
	}
}

func TestReconcileRejectsUnusableUploads(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	// No file at all.
	req := httptest.NewRequest(http.MethodPost, "/reconciliation", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)
	api.assertProblem(rec, http.StatusBadRequest, "statement_required")

	// A file with no header.
	api.assertProblem(api.uploadStatement("empty.csv", ""),
		http.StatusBadRequest, "statement_unreadable")

	// A file missing required columns.
	api.assertProblem(api.uploadStatement("short.csv", "external_ref,amount\nTRX-1,100\n"),
		http.StatusBadRequest, "statement_unreadable")
}

func TestGetUnknownReconciliationRunIs404(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	api.assertProblem(
		api.do(http.MethodGet, "/reconciliation/2a1c4f6e-0000-4000-8000-000000000000", nil),
		http.StatusNotFound, "reconciliation_run_not_found")
}

// Pagination must report the end exactly. A run whose findings divide evenly by
// the page size previously handed back a cursor leading to an empty page.
func TestReconciliationPaginationReportsTheEndExactly(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 10_000)

	// Four ghost rows plus the unmatched funding transfer: five findings.
	csv := csvHeader
	for i := range 4 {
		csv += fmt.Sprintf("GHOST-%d,2026-09-01T10:00:00Z,%s,%s,%d,BRL\n", i, bob, alice, 100+i)
	}

	rec := api.uploadStatement("ghosts.csv", csv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}
	var run runBody
	api.decode(rec, &run)
	total := run.DiscrepancyCount
	if total == 0 {
		t.Fatal("expected discrepancies to paginate over")
	}

	// Walk every page and collect the findings.
	var (
		seen   int
		cursor int64
		pages  int
	)
	for {
		pages++
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		url := fmt.Sprintf("/reconciliation/%s?limit=1&after=%d", run.ID, cursor)
		rec := api.do(http.MethodGet, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", pages, rec.Code, rec.Body)
		}
		var page runBody
		api.decode(rec, &page)

		seen += len(page.Discrepancies)
		if page.NextCursor == nil {
			// The last page must not be empty: an exact end means the cursor
			// stopped one page earlier than a naive full-page check would.
			if len(page.Discrepancies) == 0 && seen < total {
				t.Error("pagination ended on an empty page")
			}
			break
		}
		if len(page.Discrepancies) == 0 {
			t.Fatal("a page with a next cursor returned no findings")
		}
		cursor = *page.NextCursor
	}

	if seen != total {
		t.Errorf("paged through %d findings, want %d", seen, total)
	}
	// limit=1 over N findings must take exactly N pages, not N+1.
	if pages != total {
		t.Errorf("took %d pages for %d findings at limit=1, want %d", pages, total, total)
	}
}
