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

// The findings cap must never discard the integrity checks: drift is what keeps
// the materialized balance honest, and dropping it because a statement happened
// to be messy would silence the check exactly when the data is least trusted.
func TestFindingsCapNeverDropsIntegrityFindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 500)

	// Corrupt a cached balance so a drift finding must be produced.
	if _, err := f.pool.Exec(ctx,
		`UPDATE accounts SET balance = balance + 7 WHERE id = $1`, f.bob); err != nil {
		t.Fatalf("corrupt balance: %v", err)
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
	if kinds[reconcile.KindBalanceDrift] != 1 {
		t.Errorf("balance_drift = %d, want 1 -- the integrity check was dropped by the "+
			"findings cap (kinds: %v)", kinds[reconcile.KindBalanceDrift], kinds)
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
			if legs, ok := d.Details["legs"].(int); ok && legs == 0 {
				found = true
			}
			if legs, ok := d.Details["legs"].(int64); ok && legs == 0 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("a transaction with zero legs was not reported (kinds: %v)", kindsOf(run))
	}
}
