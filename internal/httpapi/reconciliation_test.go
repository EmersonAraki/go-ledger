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

	"github.com/google/uuid"

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
	// In the ledger and on the statement, but in a different currency.
	wrongCurrency := api.postTransferWithRef(bob, alice, 500, "TRX-CURRENCY")
	// In the ledger and on the statement, but dated far outside the tolerance.
	wrongDate := api.postTransferWithRef(bob, alice, 600, "TRX-DATE")
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
		// currency_mismatch
		fmt.Sprintf("TRX-CURRENCY,%s,%s,%s,500,USD\n", at(wrongCurrency), bob, alice) +
		// date_mismatch -- beyond the 24h tolerance, so the referenced pair still
		// matches but the dates are reported as disagreeing.
		fmt.Sprintf("TRX-DATE,%s,%s,%s,600,BRL\n",
			api.postedAt(wrongDate).Add(-40*time.Hour).Format(time.RFC3339), bob, alice) +
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
		reconcile.KindCurrencyMismatch,
		reconcile.KindDateMismatch,
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

// A statement covering one period must not report activity from a neighbouring
// period as missing. The ledger query deliberately widens its range by the date
// tolerance so a near-miss can still be paired -- but that widening is for
// pairing only. Reporting on it turns every adjacent day of normal activity into
// noise, which is how a reconciliation report becomes something operators learn
// to ignore.
func TestReconcileDoesNotReportActivityOutsideTheStatementWindow(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 10_000)

	// Two transfers. The statement will name only the second.
	older := api.postTransferWithRef(bob, alice, 100, "TRX-OLD")
	recent := api.postTransferWithRef(bob, alice, 200, "TRX-NEW")

	// Backdate by exactly one day. This value is load-bearing in two directions
	// and neither is obvious:
	//   - A full 24h always lands on the previous UTC day whatever hour the test
	//     runs at, so the assertion is not time-of-day dependent. A sub-day
	//     offset would be.
	//   - It stays INSIDE the +/-24h that loadLedgerWindow widens by, so the
	//     unfixed matcher loads these transactions and misreports them. Backdate
	//     further and they are never loaded at all, the old code has nothing to
	//     over-report, and this test silently asserts nothing.
	fundingID := api.transactionIDForAmount(10_000)
	for _, id := range []string{older, fundingID} {
		if _, err := api.pool.Exec(context.Background(),
			`UPDATE transactions SET created_at = created_at - interval '1 day' WHERE id = $1`,
			id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	csv := csvHeader + fmt.Sprintf("TRX-NEW,%s,%s,%s,200,BRL\n",
		api.postedAt(recent).Format(time.RFC3339), bob, alice)

	rec := api.uploadStatement("one-day.csv", csv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)

	if run.MatchedCount != 1 {
		t.Errorf("matched_count = %d, want 1", run.MatchedCount)
	}
	if got := run.kinds()[reconcile.KindMissingInStatement]; got != 0 {
		t.Errorf("missing_in_statement = %d, want 0 -- transactions outside the "+
			"statement's window were reported as unaccounted for (all kinds: %v)",
			got, run.kinds())
	}
	if !run.Clean {
		t.Errorf("expected a clean run, got %d findings: %v", run.DiscrepancyCount, run.kinds())
	}
}

// The pagination query parameters are new validation surface with four 400
// branches and no coverage.
func TestReconciliationRejectsBadPaginationParameters(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	rec := api.uploadStatement("s.csv", csvHeader+
		fmt.Sprintf("TRX-GHOST,2026-09-01T10:00:00Z,%s,%s,100,BRL\n", bob, alice))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}
	var run runBody
	api.decode(rec, &run)

	for name, tc := range map[string]struct{ query, typ string }{
		"non-numeric cursor": {"?after=abc", "invalid_cursor"},
		"negative cursor":    {"?after=-1", "invalid_cursor"},
		"zero limit":         {"?limit=0", "invalid_limit"},
		"oversized limit":    {"?limit=501", "invalid_limit"},
		"non-numeric limit":  {"?limit=many", "invalid_limit"},
	} {
		t.Run(name, func(t *testing.T) {
			api.assertProblem(
				api.do(http.MethodGet, "/reconciliation/"+run.ID+tc.query, nil),
				http.StatusBadRequest, tc.typ)
		})
	}

	// The boundaries themselves must be accepted.
	for _, ok := range []string{"?limit=1", "?limit=500", "?after=0"} {
		if rec := api.do(http.MethodGet, "/reconciliation/"+run.ID+ok, nil); rec.Code != http.StatusOK {
			t.Errorf("%s was rejected: status %d, body %s", ok, rec.Code, rec.Body)
		}
	}
}

// A multi-leg transaction cannot be expressed on a two-column statement. It must
// still be reported: silently excluding it would let the job announce a clean
// period while money it never examined had moved.
func TestReconcileReportsTransactionsItCannotExpress(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	carol := api.createAccount("carol", "BRL", false)
	api.fund(alice, "BRL", 10_000)

	// Write a balanced three-leg transaction directly: the API only exposes the
	// two-leg case, but the schema has supported N legs since migration 0001.
	ctx := context.Background()
	txID := uuid.NewString()

	dbTx, err := api.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = dbTx.Rollback(context.Background()) }()

	if _, err := dbTx.Exec(ctx,
		`INSERT INTO transactions (id, currency, external_ref) VALUES ($1,'BRL','TRX-SPLIT')`,
		txID); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	for _, e := range [][]any{
		{uuid.NewString(), txID, bob, "debit", int64(300)},
		{uuid.NewString(), txID, carol, "debit", int64(200)},
		{uuid.NewString(), txID, alice, "credit", int64(500)},
	} {
		if _, err := dbTx.Exec(ctx, `
			INSERT INTO ledger_entries (id, transaction_id, account_id, direction, amount, currency)
			VALUES ($1,$2,$3,$4,$5,'BRL')`, e...); err != nil {
			t.Fatalf("insert entry: %v", err)
		}
	}
	// The zero-sum trigger is DEFERRABLE INITIALLY DEFERRED, so the three legs
	// are only checked here, together: 300 + 200 - 500 = 0.
	if err := dbTx.Commit(ctx); err != nil {
		t.Fatalf("commit three-leg transaction: %v", err)
	}

	rec := api.uploadStatement("split.csv", csvHeader+
		fmt.Sprintf("FUND-%s,%s,%s,%s,10000,BRL\n", alice[:8],
			api.postedAt(api.transactionIDForAmount(10_000)).Format(time.RFC3339),
			alice, api.fundingSource))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)
	if got := run.kinds()[reconcile.KindUnreconcilableTransaction]; got != 1 {
		t.Errorf("unreconcilable_transaction = %d, want 1 -- a three-leg transaction "+
			"was silently excluded (all kinds: %v)", got, run.kinds())
	}
	for _, d := range run.Discrepancies {
		if d.Kind == reconcile.KindUnreconcilableTransaction {
			if legs, _ := d.Details["legs"].(float64); legs != 3 {
				t.Errorf("legs = %v, want 3", d.Details["legs"])
			}
		}
	}
}

// The cursor POST hands back must mean the same thing GET expects. GET paginates
// on the database row id; discrepancy ids come from a global sequence, so the
// Nth finding of a run is not row id N. A cursor that conflates the two makes a
// client re-receive findings it already has.
func TestPostCursorIsUsableAgainstGet(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 100_000)

	// A first run, purely to advance the shared id sequence so that the second
	// run's row ids cannot coincide with its own 1..N ordinals.
	first := csvHeader
	for i := range 5 {
		first += fmt.Sprintf("PRIOR-%d,2026-09-01T10:00:00Z,%s,%s,%d,BRL\n", i, bob, alice, 10+i)
	}
	if rec := api.uploadStatement("prior.csv", first); rec.Code != http.StatusCreated {
		t.Fatalf("first upload: status %d, body %s", rec.Code, rec.Body)
	}

	// A second run with more findings than one page.
	second := csvHeader
	for i := range 150 {
		second += fmt.Sprintf("GHOST-%d,2026-09-02T10:00:00Z,%s,%s,%d,BRL\n", i, bob, alice, 1000+i)
	}
	rec := api.uploadStatement("many.csv", second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second upload: status %d, body %s", rec.Code, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)
	if run.NextCursor == nil {
		t.Fatalf("expected a truncated POST response with a cursor; got %d findings",
			len(run.Discrepancies))
	}

	// Walk from POST's cursor through GET, collecting what the client would see.
	seen := map[string]int{}
	key := func(d struct {
		Kind          string         `json:"kind"`
		StatementRef  *string        `json:"statement_ref"`
		TransactionID *string        `json:"transaction_id"`
		Details       map[string]any `json:"details"`
	}) string {
		ref := ""
		if d.StatementRef != nil {
			ref = *d.StatementRef
		}
		return fmt.Sprintf("%s|%s|%v", d.Kind, ref, d.Details["line"])
	}

	for _, d := range run.Discrepancies {
		seen[key(d)]++
	}

	cursor := *run.NextCursor
	for page := 0; ; page++ {
		if page > 30 {
			t.Fatal("pagination did not terminate")
		}
		rec := api.do(http.MethodGet,
			fmt.Sprintf("/reconciliation/%s?after=%d", run.ID, cursor), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", page, rec.Code, rec.Body)
		}
		var p runBody
		api.decode(rec, &p)
		for _, d := range p.Discrepancies {
			seen[key(d)]++
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}

	var duplicated int
	for k, n := range seen {
		if n > 1 {
			duplicated++
			if duplicated <= 3 {
				t.Errorf("finding %q was delivered %d times", k, n)
			}
		}
	}
	if duplicated > 0 {
		t.Errorf("%d findings were delivered more than once: POST's cursor does not "+
			"mean what GET's does", duplicated)
	}
	if len(seen) != run.DiscrepancyCount {
		t.Errorf("client saw %d distinct findings, run reports %d",
			len(seen), run.DiscrepancyCount)
	}
}

// The comparison window comes from the file's own dates, so it is client
// controlled. A tiny upload claiming to span centuries would otherwise make the
// ledger query load every transaction that has ever existed.
func TestReconcileRejectsAnImplausibleWindow(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	csv := csvHeader +
		fmt.Sprintf("TRX-ANCIENT,1970-01-01,%s,%s,1,BRL\n", bob, alice) +
		fmt.Sprintf("TRX-FUTURE,2100-01-01,%s,%s,1,BRL\n", bob, alice)

	api.assertProblem(api.uploadStatement("centuries.csv", csv),
		http.StatusBadRequest, "statement_window_too_wide")

	// A plausible span is still accepted.
	ok := csvHeader +
		fmt.Sprintf("TRX-A,2026-09-01,%s,%s,1,BRL\n", bob, alice) +
		fmt.Sprintf("TRX-B,2026-09-30,%s,%s,1,BRL\n", bob, alice)
	if rec := api.uploadStatement("month.csv", ok); rec.Code != http.StatusCreated {
		t.Errorf("a one-month statement was rejected: status %d, body %s", rec.Code, rec.Body)
	}
}

// ADR 0003 forbids floats for money. The stored details travel through JSON, and
// decoding into map[string]any turns numbers into float64 unless asked not to --
// so the same finding could report different amounts from the two endpoints.
func TestReconciliationAmountsSurviveBothEndpointsExactly(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)

	// Beyond 2^53, where float64 stops being able to represent consecutive
	// integers. 9007199254740993 rounds to ...992 if it passes through a float.
	const huge = "9007199254740993"

	csv := csvHeader +
		fmt.Sprintf("TRX-HUGE,2026-09-01T10:00:00Z,%s,%s,%s,BRL\n", bob, alice, huge)

	rec := api.uploadStatement("huge.csv", csv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(huge)) {
		t.Fatalf("POST did not return the exact amount %s: %s", huge, rec.Body)
	}

	var run runBody
	api.decode(rec, &run)

	got := api.do(http.MethodGet, "/reconciliation/"+run.ID, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", got.Code, got.Body)
	}
	if !bytes.Contains(got.Body.Bytes(), []byte(huge)) {
		t.Errorf("GET lost precision on a monetary amount: expected %s in\n%s",
			huge, got.Body)
	}
}
