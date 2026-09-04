package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

// corruptBalance moves an account's cached balance away from the sum of its
// entries, exactly as a bug elsewhere in the system would. SUM(signed_amount) is
// untouched and remains the truth.
func (f *recFixture) corruptBalance(ctx context.Context, t *testing.T, id uuid.UUID, by int64) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`UPDATE accounts SET balance = balance + $2 WHERE id = $1`, id, by); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}
}

func TestSweepDetectsBalanceDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 300)
	f.corruptBalance(ctx, t, f.bob, 42)

	run, err := f.store.SweepBalanceDrift(ctx, reconcile.DefaultDriftPageSize)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if run == nil {
		t.Fatal("a corrupted balance was not detected")
	}
	if run.SourceName != reconcile.DriftSweepSource {
		t.Errorf("source = %q, want %q", run.SourceName, reconcile.DriftSweepSource)
	}

	var found int
	for _, d := range run.Discrepancies {
		if d.Kind != reconcile.KindBalanceDrift {
			continue
		}
		found++
		if name, _ := d.Details["account_name"].(string); name != "bob" {
			t.Errorf("drift reported for %q, want bob", name)
		}
		if diff, _ := d.Details["difference"].(int64); diff != 42 {
			t.Errorf("difference = %v, want 42", d.Details["difference"])
		}
	}
	if found != 1 {
		t.Errorf("balance_drift findings = %d, want exactly 1 (%d accounts checked)",
			found, run.StatementRows)
	}

	// The run is persisted, so an operator reads it back over the API rather
	// than from the job's logs.
	stored, err := f.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.SourceName != reconcile.DriftSweepSource {
		t.Errorf("stored source = %q, want %q", stored.SourceName, reconcile.DriftSweepSource)
	}
}

// A clean ledger must record nothing at all. A run row means something is wrong,
// so a sweep that wrote one every time would make the signal meaningless.
func TestSweepRecordsNothingWhenEveryBalanceAgrees(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 300)

	run, err := f.store.SweepBalanceDrift(ctx, reconcile.DefaultDriftPageSize)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if run != nil {
		t.Fatalf("a clean ledger produced a run with %d findings", run.DiscrepancyCount)
	}

	var runs int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM reconciliation_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("reconciliation_runs = %d, want 0 for a clean sweep", runs)
	}
}

// The keyset cursor has to work against real uuid ordering, not just the fake
// scanner's. With a page size of one, drift on the last account is only found if
// every page boundary is crossed correctly.
func TestSweepPagesAcrossEveryAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	// The fixture already holds three accounts; add enough that a page size of
	// one needs many crossings, and corrupt several spread through the ordering.
	ids := []uuid.UUID{f.alice, f.bob, f.source}
	for i := range 8 {
		ids = append(ids, newAccount(ctx, t, f.pool, fmt.Sprintf("acct-%d", i), 0, true))
	}
	// Corrupting by id order is the point: whichever of these lands last in the
	// uuid ordering is the one a broken cursor would miss.
	corrupted := map[uuid.UUID]bool{}
	for _, id := range []uuid.UUID{ids[0], ids[4], ids[len(ids)-1]} {
		f.corruptBalance(ctx, t, id, 5)
		corrupted[id] = true
	}

	run, err := f.store.SweepBalanceDrift(ctx, 1)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if run == nil {
		t.Fatal("no drift found; three accounts were corrupted")
	}
	if run.StatementRows != len(ids) {
		t.Errorf("accounts checked = %d, want %d -- the sweep did not reach every page",
			run.StatementRows, len(ids))
	}

	seen := map[string]bool{}
	for _, d := range run.Discrepancies {
		if d.Kind == reconcile.KindBalanceDrift {
			seen[fmt.Sprint(d.Details["account_id"])] = true
		}
	}
	for id := range corrupted {
		if !seen[id.String()] {
			t.Errorf("drift on account %s was missed (found: %v)", id, seen)
		}
	}
	if len(seen) != len(corrupted) {
		t.Errorf("drift findings = %d, want %d", len(seen), len(corrupted))
	}
}

// The regression this whole change exists for. Reconcile once summed every
// account's entire history on every upload, so a one-row CSV -- the smallest
// possible request on an unauthenticated endpoint -- paid for a full ledger
// scan. Scoping that query to the statement's window did not bound it: the
// accounts it selected still had their whole history summed, measurably slower
// than the query it replaced. A statement run must not do this work at all.
func TestStatementRunNeverReportsBalanceDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRecFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 300)
	// Corrupt an account the statement squarely covers, so nothing about the
	// window can explain the absence of a finding.
	f.corruptBalance(ctx, t, f.bob, 42)

	now := time.Now().UTC().Format(time.RFC3339)
	stmt := parse(t, recCSVHeader+
		fmt.Sprintf("TRX-1,%s,%s,%s,300,BRL\n", now, f.bob, f.alice))

	run, err := f.store.Reconcile(ctx, "s.csv", stmt, reconcile.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := kindsOf(run)[reconcile.KindBalanceDrift]; got != 0 {
		t.Errorf("balance_drift = %d, want 0 -- a statement upload swept the ledger "+
			"(kinds: %v)", got, kindsOf(run))
	}

	// And the drift is genuinely still caught, by the job that can afford it.
	sweep, err := f.store.SweepBalanceDrift(ctx, reconcile.DefaultDriftPageSize)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep == nil {
		t.Error("the drift left the request path and is now caught nowhere")
	}
}
