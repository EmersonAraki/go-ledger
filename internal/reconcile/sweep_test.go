package reconcile_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

// fakeScanner serves a fixed list of accounts through the keyset contract, so
// the paging logic can be tested at boundaries a real ledger would need
// thousands of rows to reach.
type fakeScanner struct {
	accounts []reconcile.BalanceDrift // in id order; drifting ones have Cached != Derived
	calls    int
	// stuck makes every page report the same cursor, the one failure that would
	// otherwise hang the sweep forever.
	stuck bool
}

func (f *fakeScanner) ScanDriftPage(_ context.Context, after *uuid.UUID, limit int) (reconcile.DriftPage, error) {
	f.calls++

	var page reconcile.DriftPage
	for _, a := range f.accounts {
		if after != nil && a.AccountID.String() <= after.String() {
			continue
		}
		page.Scanned++
		page.LastID = a.AccountID
		if a.Cached != a.Derived {
			page.Drifts = append(page.Drifts, a)
		}
		if page.Scanned == limit {
			break
		}
	}
	if f.stuck && after != nil {
		page.LastID = *after
	}
	return page, nil
}

// accountsWithDrift builds n accounts with ascending, comparable ids, the first
// `drifting` of which disagree with their entries.
func accountsWithDrift(n, drifting int) []reconcile.BalanceDrift {
	out := make([]reconcile.BalanceDrift, 0, n)
	for i := range n {
		// Sortable ids, so "id > cursor" behaves like the SQL it stands in for.
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1))
		d := reconcile.BalanceDrift{AccountID: id, Name: fmt.Sprintf("a-%d", i), Cached: 10, Derived: 10}
		if i < drifting {
			d.Derived = 3
		}
		out = append(out, d)
	}
	return out
}

// The sweep must page through every account, not stop at the first page. This is
// the whole point of the keyset cursor: a bug that dropped it would silently
// check only the first 500 accounts and report the ledger clean.
func TestSweepDriftPagesThroughEveryAccount(t *testing.T) {
	t.Parallel()

	// 7 accounts, 3 to a page: three full pages and a short one.
	f := &fakeScanner{accounts: accountsWithDrift(7, 2)}
	sweep, err := reconcile.SweepDrift(context.Background(), f, 3)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Scanned != 7 {
		t.Errorf("scanned = %d, want 7 -- the sweep stopped short of the last page", sweep.Scanned)
	}
	if len(sweep.Drifts) != 2 {
		t.Errorf("drifts = %d, want 2", len(sweep.Drifts))
	}
	if f.calls != 3 {
		t.Errorf("pages fetched = %d, want 3 (7 accounts, 3 per page)", f.calls)
	}
	if sweep.Truncated {
		t.Error("sweep reported truncation with 2 findings")
	}
}

// A page exactly filling the requested size is not the end of the data. Getting
// this backwards silently drops every account past the boundary.
func TestSweepDriftContinuesPastAnExactlyFullPage(t *testing.T) {
	t.Parallel()

	f := &fakeScanner{accounts: accountsWithDrift(6, 1)}
	sweep, err := reconcile.SweepDrift(context.Background(), f, 3)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Scanned != 6 {
		t.Errorf("scanned = %d, want 6", sweep.Scanned)
	}
	// Two full pages, then one empty page proving there was nothing more.
	if f.calls != 3 {
		t.Errorf("pages fetched = %d, want 3", f.calls)
	}
}

// A cursor that stops advancing must end the sweep with an error rather than
// loop forever. A hung job is far harder to diagnose than a failed one.
func TestSweepDriftFailsOnAStuckCursor(t *testing.T) {
	t.Parallel()

	f := &fakeScanner{accounts: accountsWithDrift(10, 0), stuck: true}
	_, err := reconcile.SweepDrift(context.Background(), f, 3)
	if err == nil {
		t.Fatal("a cursor that never advances was accepted; the sweep would hang")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("error = %q, want it to name the stuck cursor", err)
	}
}

func TestSweepDriftTruncatesAtTheFindingsCap(t *testing.T) {
	t.Parallel()

	// Every account drifts, so the cap is reached before the accounts run out.
	n := reconcile.MaxDriftFindings + 10
	f := &fakeScanner{accounts: accountsWithDrift(n, n)}
	sweep, err := reconcile.SweepDrift(context.Background(), f, 1000)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !sweep.Truncated {
		t.Error("truncated = false with more drift than the cap allows")
	}
	if len(sweep.Drifts) != reconcile.MaxDriftFindings {
		t.Errorf("drifts = %d, want exactly the cap %d",
			len(sweep.Drifts), reconcile.MaxDriftFindings)
	}
}

// Exactly the cap's worth of drift is not truncation: nothing was dropped, and
// claiming otherwise sends an operator looking for findings that do not exist.
func TestSweepDriftDoesNotClaimTruncationAtExactlyTheCap(t *testing.T) {
	t.Parallel()

	n := reconcile.MaxDriftFindings
	f := &fakeScanner{accounts: accountsWithDrift(n, n)}
	sweep, err := reconcile.SweepDrift(context.Background(), f, 1000)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Truncated {
		t.Error("truncated = true with exactly the cap's findings and nothing dropped")
	}
	if len(sweep.Drifts) != n {
		t.Errorf("drifts = %d, want %d", len(sweep.Drifts), n)
	}
}

// A clean sweep must produce nothing to report: no drift, no truncation.
func TestSweepDriftReportsNothingWhenEveryBalanceAgrees(t *testing.T) {
	t.Parallel()

	f := &fakeScanner{accounts: accountsWithDrift(5, 0)}
	sweep, err := reconcile.SweepDrift(context.Background(), f, 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(sweep.Drifts) != 0 || sweep.Truncated {
		t.Errorf("clean ledger reported %d drifts (truncated=%v)", len(sweep.Drifts), sweep.Truncated)
	}
	if sweep.Scanned != 5 {
		t.Errorf("scanned = %d, want 5 -- a clean sweep must still cover every account", sweep.Scanned)
	}
}

// DriftRun must carry the truncation forward as a finding. Silently reporting
// 10,000 drifting accounts as if that were the whole story is the failure mode.
func TestDriftRunRecordsTruncation(t *testing.T) {
	t.Parallel()

	run := reconcile.DriftRun(reconcile.DriftSweep{
		Drifts:    accountsWithDrift(2, 2),
		Scanned:   9,
		Truncated: true,
	})
	if run.SourceName != reconcile.DriftSweepSource {
		t.Errorf("source = %q, want %q", run.SourceName, reconcile.DriftSweepSource)
	}
	kinds := map[string]int{}
	for _, d := range run.Discrepancies {
		kinds[d.Kind]++
	}
	if kinds[reconcile.KindBalanceDrift] != 2 {
		t.Errorf("balance_drift = %d, want 2 (kinds: %v)", kinds[reconcile.KindBalanceDrift], kinds)
	}
	if kinds[reconcile.KindFindingsTruncated] != 1 {
		t.Errorf("findings_truncated = %d, want 1 (kinds: %v)",
			kinds[reconcile.KindFindingsTruncated], kinds)
	}
	if run.DiscrepancyCount != len(run.Discrepancies) {
		t.Errorf("discrepancy_count = %d but %d findings", run.DiscrepancyCount, len(run.Discrepancies))
	}
	if run.MatchedCount != 7 {
		t.Errorf("matched = %d, want 7 (9 scanned, 2 drifting)", run.MatchedCount)
	}
}

// The caller-supplied limits may only lower the computed caps, never raise them.
// They exist so tests can reach the boundaries; a caller that could raise them
// would be a caller that could remove the bounds these limits are.
func TestOptionsCannotRaiseTheLimits(t *testing.T) {
	t.Parallel()

	opts := reconcile.Options{
		MaxFindings:   reconcile.MaxFindings * 10,
		MaxLedgerRows: reconcile.MaxLedgerWindowRows * 10,
	}
	if got := opts.FindingsLimit(); got != reconcile.MaxFindings {
		t.Errorf("FindingsLimit = %d, want it clamped to %d", got, reconcile.MaxFindings)
	}
	// A huge statement earns the ceiling; the option must not lift it further.
	if got := opts.LedgerRowsLimit(reconcile.MaxLedgerWindowRows); got != reconcile.MaxLedgerWindowRows {
		t.Errorf("LedgerRowsLimit = %d, want it clamped to %d",
			got, reconcile.MaxLedgerWindowRows)
	}
}

// LedgerRowsLimit is what Reconcile actually calls, so it -- not just the
// underlying LedgerRowsFor -- has to track the upload. Asserting only on
// LedgerRowsFor left the accessor free to return the ceiling and no test noticed.
func TestLedgerRowsLimitTracksTheUpload(t *testing.T) {
	t.Parallel()

	var opts reconcile.Options
	for _, rows := range []int{0, 1, 2, 50} {
		got := opts.LedgerRowsLimit(rows)
		if want := reconcile.LedgerRowsFor(rows); got != want {
			t.Errorf("LedgerRowsLimit(%d) = %d, want %d", rows, got, want)
		}
		if got >= reconcile.MaxLedgerWindowRows {
			t.Errorf("LedgerRowsLimit(%d) = %d, at or above the ceiling %d -- a small "+
				"upload is buying a ledger-sized scan", rows, got, reconcile.MaxLedgerWindowRows)
		}
	}
}

// The ledger row cap must be a function of the upload. A constant sized off the
// ledger is not a bound -- it is a bigger ledger's problem deferred.
func TestLedgerRowsForScalesWithTheUpload(t *testing.T) {
	t.Parallel()

	small := reconcile.LedgerRowsFor(2)
	large := reconcile.LedgerRowsFor(2_000)
	if small >= large {
		t.Errorf("LedgerRowsFor(2) = %d is not below LedgerRowsFor(2000) = %d; "+
			"the cap does not track the upload", small, large)
	}
	if small != reconcile.MinLedgerWindowRows+2*reconcile.LedgerRowsPerStatementRow {
		t.Errorf("LedgerRowsFor(2) = %d, want floor %d plus two rows' allowance",
			small, reconcile.MinLedgerWindowRows)
	}
	// The ceiling still holds, including against an overflow-sized row count.
	for _, n := range []int{reconcile.MaxLedgerWindowRows, 1 << 40, math.MaxInt} {
		if got := reconcile.LedgerRowsFor(n); got != reconcile.MaxLedgerWindowRows {
			t.Errorf("LedgerRowsFor(%d) = %d, want the ceiling %d",
				n, got, reconcile.MaxLedgerWindowRows)
		}
	}
	// A header-only statement still gets the floor, never a negative cap.
	if got := reconcile.LedgerRowsFor(0); got != reconcile.MinLedgerWindowRows {
		t.Errorf("LedgerRowsFor(0) = %d, want the floor %d", got, reconcile.MinLedgerWindowRows)
	}
}
