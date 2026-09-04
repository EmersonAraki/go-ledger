package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/ledger"
	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
	"github.com/EmersonAraki/go-ledger/internal/reconcile"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// These tests drive the reconciliation limits directly, at boundaries the HTTP
// tests cannot reach: the real caps are 10,000 findings and 200,000 ledger rows,
// so a test that uses them exercises nothing. Untested limits are how several
// defects in this file survived earlier review rounds.

const recCSVHeader = "external_ref,posted_at,debit_account_id,credit_account_id,amount,currency\n"

type recFixture struct {
	pool   *pgxpool.Pool
	store  *postgres.Store
	alice  uuid.UUID
	bob    uuid.UUID
	source uuid.UUID
}

func newRecFixture(ctx context.Context, t *testing.T) *recFixture {
	t.Helper()
	pool := pgtest.Pool(t)
	f := &recFixture{
		pool:   pool,
		store:  postgres.NewStore(pool),
		alice:  newAccount(ctx, t, pool, "alice", 0, false),
		bob:    newAccount(ctx, t, pool, "bob", 0, false),
		source: newAccount(ctx, t, pool, "external", 0, true),
	}
	// Fund alice so later transfers have money to move.
	f.transfer(ctx, t, f.alice, f.source, 100_000)
	return f
}

// transfer posts a transfer through the store, which is what the reconciliation
// queries then read back.
func (f *recFixture) transfer(ctx context.Context, t *testing.T, debit, credit uuid.UUID, amount int64) {
	t.Helper()

	claim := ledger.Claim{
		Key:         uuid.NewString(),
		Endpoint:    "POST /transactions",
		Fingerprint: []byte(uuid.NewString()),
	}
	render := func(tx *ledger.Transaction) (int, []byte, error) {
		return 201, []byte(`{"id":"` + tx.ID.String() + `"}`), nil
	}
	if _, err := f.store.Transfer(ctx, ledger.TransferCommand{
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          amount,
		Currency:        "BRL",
	}, claim, render); err != nil {
		t.Fatalf("transfer: %v", err)
	}
}

// transferWithRef posts a transfer carrying an external reference, so a
// statement row can match it exactly.
func (f *recFixture) transferWithRef(ctx context.Context, t *testing.T,
	debit, credit uuid.UUID, amount int64, ref string) {
	t.Helper()

	claim := ledger.Claim{
		Key:         uuid.NewString(),
		Endpoint:    "POST /transactions",
		Fingerprint: []byte(uuid.NewString()),
	}
	render := func(tx *ledger.Transaction) (int, []byte, error) {
		return 201, []byte(`{"id":"` + tx.ID.String() + `"}`), nil
	}
	if _, err := f.store.Transfer(ctx, ledger.TransferCommand{
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          amount,
		Currency:        "BRL",
		ExternalRef:     &ref,
	}, claim, render); err != nil {
		t.Fatalf("transfer with ref %s: %v", ref, err)
	}
}

// parse turns a CSV into a Statement the store can reconcile.
func parse(t *testing.T, csv string) reconcile.Statement {
	t.Helper()
	stmt, err := reconcile.ParseStatement(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse statement: %v", err)
	}
	return stmt
}

func kindsOf(run *reconcile.Run) map[string]int {
	out := map[string]int{}
	for _, d := range run.Discrepancies {
		out[d.Kind]++
	}
	return out
}

// The ledger load is limited, and hitting the limit is reported rather than
// silently producing a partial comparison that looks complete.
func TestReconcileReportsLedgerTruncation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// Several transfers in the window (plus the funding one), but only two may
	// be loaded.
	for range 3 {
		f.transfer(ctx, t, f.bob, f.alice, 100)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stmt := parse(t, recCSVHeader+
		fmt.Sprintf("TRX-NONE,%s,%s,%s,1,BRL\n", now, f.bob, f.alice))

	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{MaxLedgerRows: 2})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := kindsOf(run)[reconcile.KindLedgerTruncated]; got != 1 {
		t.Errorf("ledger_truncated = %d, want 1 (kinds: %v)", got, kindsOf(run))
	}

	// Under the limit it must not be reported.
	run, err = f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{MaxLedgerRows: 50})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := kindsOf(run)[reconcile.KindLedgerTruncated]; got != 0 {
		t.Errorf("ledger_truncated = %d, want 0 under the limit", got)
	}
}

// The findings cap must never discard the integrity checks: an unreconcilable
// transaction is a ledger problem, and dropping it because a statement happened
// to be messy would silence the check exactly when the data is least trusted.
func TestFindingsCapNeverDropsIntegrityFindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 500)

	// A legless transaction inside the window: an integrity finding the
	// statement comparison did not produce and must not be able to crowd out.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO transactions (id, currency) VALUES ($1, 'BRL')`, uuid.New()); err != nil {
		t.Fatalf("insert legless transaction: %v", err)
	}

	// Far more statement findings than the cap allows.
	now := time.Now().UTC().Format(time.RFC3339)
	csv := recCSVHeader
	for i := range 8 {
		csv += fmt.Sprintf("GHOST-%d,%s,%s,%s,%d,BRL\n", i, now, f.bob, f.alice, 900+i)
	}

	run, err := f.store.Reconcile(ctx, "s.csv", parse(t, csv), reconcile.Options{MaxFindings: 2})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	kinds := kindsOf(run)
	if kinds[reconcile.KindUnreconcilableTransaction] != 1 {
		t.Errorf("unreconcilable_transaction = %d, want 1 -- the integrity check was "+
			"dropped by the findings cap (kinds: %v)",
			kinds[reconcile.KindUnreconcilableTransaction], kinds)
	}
	if kinds[reconcile.KindFindingsTruncated] != 1 {
		t.Errorf("findings_truncated = %d, want 1 (kinds: %v)",
			kinds[reconcile.KindFindingsTruncated], kinds)
	}
	if run.DiscrepancyCount != len(run.Discrepancies) {
		t.Errorf("discrepancy_count = %d but %d findings returned",
			run.DiscrepancyCount, len(run.Discrepancies))
	}
}

// StatementRows must be the number of rows actually READ, not a count of
// findings. The two only diverge once findings are capped or synthesised, so the
// test has to push past MaxReportedParseErrors -- a small file gives the same
// number either way and would assert nothing.
func TestRunReportsTheRowsActuallyRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// Enough unparseable rows that the per-row findings are suppressed and a
	// synthetic truncation finding is added: len(findings) is then far from the
	// number of rows read.
	const badRows = reconcile.MaxReportedParseErrors + 200

	var b strings.Builder
	b.WriteString(recCSVHeader)
	now := time.Now().UTC().Format(time.RFC3339)
	b.WriteString(fmt.Sprintf("TRX-GOOD,%s,%s,%s,100,BRL\n", now, f.bob, f.alice))
	for i := range badRows {
		fmt.Fprintf(&b, "TRX-BAD-%d,not-a-date,%s,%s,1,BRL\n", i, f.bob, f.alice)
	}

	stmt := parse(t, b.String())
	wantRows := badRows + 1

	// Guard the premise: if these ever coincide the assertion below is vacuous.
	if len(stmt.Rows)+len(stmt.Findings) == wantRows {
		t.Fatalf("test premise broken: findings count (%d) equals rows read (%d), "+
			"so this cannot distinguish the two", len(stmt.Rows)+len(stmt.Findings), wantRows)
	}

	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if run.StatementRows != wantRows {
		t.Errorf("statement_rows = %d, want %d -- the summary is counting findings "+
			"rather than rows, and contradicts its own truncation finding",
			run.StatementRows, wantRows)
	}
}

// A transaction with no legs at all is permitted by the zero-sum trigger, and
// must not be invisible to the check that exists so nothing is invisible.
func TestZeroLegTransactionIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 100)

	// The trigger fires per entry row, so a transaction with none never trips it.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO transactions (id, currency) VALUES ($1, 'BRL')`, uuid.New()); err != nil {
		t.Fatalf("insert legless transaction: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stmt := parse(t, recCSVHeader+
		fmt.Sprintf("TRX-NONE,%s,%s,%s,1,BRL\n", now, f.bob, f.alice))

	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var found bool
	for _, d := range run.Discrepancies {
		if d.Kind == reconcile.KindUnreconcilableTransaction {
			legs, ok := d.Details["legs"].(int)
			if !ok {
				t.Fatalf("legs is %T, want int", d.Details["legs"])
			}
			if legs == 0 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("a transaction with zero legs was not reported (kinds: %v)", kindsOf(run))
	}
}

// The unreconcilable scan's bound has to be a DATABASE bound, not a Go-side
// slice. The earlier version capped the returned slice while the SQL still
// aggregated the whole window -- deleting its LIMIT changed nothing observable,
// so nothing tested the half that protects the database.
//
// This pins the real behaviour: the scan examines the FIRST maxRows
// transactions of the window, so a malformed one past that point is not found
// and the run says so. Delete the LIMIT and it is found, and this fails.
func TestUnreconcilableScanIsBoundedInTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// A fixed day, so ordering inside the window is deterministic and the
	// fixture's own funding transfer (posted at now()) falls outside it.
	day := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	const legless = 4
	for i := range legless {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO transactions (id, currency, created_at) VALUES ($1, 'BRL', $2)`,
			uuid.New(), day.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatalf("insert legless transaction %d: %v", i, err)
		}
	}

	stmt := parse(t, recCSVHeader+fmt.Sprintf("TRX-NONE,%s,%s,%s,1,BRL\n",
		day.Add(2*time.Hour).Format(time.RFC3339), f.bob, f.alice))

	// Cap of 2: the scan examines the first two transactions of the window and
	// stops, so only two of the four are found. Without the SQL LIMIT all four
	// would come back, which is exactly what this pins down.
	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{MaxLedgerRows: 2})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	kinds := kindsOf(run)
	if kinds[reconcile.KindUnreconcilableTransaction] != 2 {
		t.Errorf("unreconcilable_transaction = %d, want exactly the cap of 2 -- the "+
			"scan read past its cap, so the database-side bound is doing nothing "+
			"(kinds: %v)", kinds[reconcile.KindUnreconcilableTransaction], kinds)
	}
	if kinds[reconcile.KindUnreconcilableTruncated] != 1 {
		t.Errorf("unreconcilable_truncated = %d, want 1 -- a partial scan was "+
			"reported as a complete one (kinds: %v)",
			kinds[reconcile.KindUnreconcilableTruncated], kinds)
	}

	// With a cap past them, all four are found and nothing is truncated.
	run, err = f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	kinds = kindsOf(run)
	if kinds[reconcile.KindUnreconcilableTransaction] != legless {
		t.Errorf("unreconcilable_transaction = %d, want %d under a cap that reaches "+
			"them (kinds: %v)", kinds[reconcile.KindUnreconcilableTransaction], legless, kinds)
	}
	if kinds[reconcile.KindUnreconcilableTruncated] != 0 {
		t.Errorf("unreconcilable_truncated = %d, want 0 (kinds: %v)",
			kinds[reconcile.KindUnreconcilableTruncated], kinds)
	}
}

// The same for loadLedgerWindow: its LIMIT must bound the database, so a
// transaction sorting past the cap is genuinely not loaded and therefore not
// matched. Deleting the LIMIT lets it match, and this fails.
func TestLedgerWindowLimitIsADatabaseBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// Several transfers; the referenced one is posted last so it sorts last.
	for range 3 {
		f.transfer(ctx, t, f.bob, f.alice, 100)
	}
	f.transferWithRef(ctx, t, f.bob, f.alice, 777, "TRX-LAST")

	var postedAt time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT created_at FROM transactions WHERE external_ref = 'TRX-LAST'`).Scan(&postedAt); err != nil {
		t.Fatalf("read posted_at: %v", err)
	}
	stmt := parse(t, recCSVHeader+fmt.Sprintf("TRX-LAST,%s,%s,%s,777,BRL\n",
		postedAt.UTC().Format(time.RFC3339Nano), f.bob, f.alice))

	// Cap below its position: it is never loaded, so the statement row has
	// nothing to match against and the run reports the window as truncated.
	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{MaxLedgerRows: 2})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	kinds := kindsOf(run)
	if run.MatchedCount != 0 {
		t.Errorf("matched = %d, want 0 -- a transaction past the cap was loaded, so "+
			"the LIMIT is not bounding the database", run.MatchedCount)
	}
	if kinds[reconcile.KindLedgerTruncated] != 1 {
		t.Errorf("ledger_truncated = %d, want 1 (kinds: %v)",
			kinds[reconcile.KindLedgerTruncated], kinds)
	}

	// Uncapped, the same statement matches exactly.
	run, err = f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if run.MatchedCount != 1 {
		t.Errorf("matched = %d, want 1 without the cap (kinds: %v)",
			run.MatchedCount, kindsOf(run))
	}
}

// The row cap must be a function of the upload, not a constant sized off the
// ledger. A two-row statement must not be able to buy a 200,000-row scan.
func TestLedgerRowCapIsDerivedFromTheUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// Far more ledger than a tiny statement should be allowed to examine, if
	// the cap were per-row rather than a flat constant.
	for range 12 {
		f.transfer(ctx, t, f.bob, f.alice, 10)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stmt := parse(t, recCSVHeader+
		fmt.Sprintf("TRX-NONE,%s,%s,%s,1,BRL\n", now, f.bob, f.alice))

	// One statement row earns MinLedgerWindowRows + one row's allowance, so
	// nothing here is truncated -- the floor exists precisely so a small upload
	// against a small ledger reports a complete run.
	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := kindsOf(run)[reconcile.KindLedgerTruncated]; got != 0 {
		t.Errorf("ledger_truncated = %d, want 0 -- the floor should cover a small "+
			"ledger (kinds: %v)", got, kindsOf(run))
	}
	// And the cap itself is the derived one, not the ceiling.
	if got := reconcile.LedgerRowsFor(stmt.RowsRead); got >= reconcile.MaxLedgerWindowRows {
		t.Errorf("LedgerRowsFor(%d) = %d, want far below the ceiling %d",
			stmt.RowsRead, got, reconcile.MaxLedgerWindowRows)
	}
}
