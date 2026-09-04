package reconcile

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
)

// BalanceDrift is an account whose cached balance disagrees with the sum of its
// entries. It lives here rather than with the matcher because matching does not
// produce drift: only the sweep does.
type BalanceDrift struct {
	AccountID uuid.UUID
	Name      string
	Cached    int64
	Derived   int64
}

// DriftDiscrepancies turns balance drift findings into discrepancies.
func DriftDiscrepancies(drifts []BalanceDrift) []Discrepancy {
	out := make([]Discrepancy, 0, len(drifts))
	for _, d := range drifts {
		out = append(out, Discrepancy{
			Kind: KindBalanceDrift,
			Details: map[string]any{
				"account_id":      d.AccountID,
				"account_name":    d.Name,
				"cached_balance":  d.Cached,
				"derived_balance": d.Derived,
				"difference":      d.Cached - d.Derived,
			},
		})
	}
	return out
}

// DriftScanner reads accounts and their derived balances one bounded page at a
// time. A nil cursor starts the sweep: nil rather than the zero uuid so that an
// account whose id happened to be all zeroes would still be swept.
type DriftScanner interface {
	ScanDriftPage(ctx context.Context, after *uuid.UUID, limit int) (DriftPage, error)
}

// DriftPage is one page of the accounts table checked against its entries.
type DriftPage struct {
	// Drifts holds only the accounts in the page whose cached balance disagreed
	// with the sum of their entries -- usually none.
	Drifts []BalanceDrift
	// Scanned is how many accounts the page covered, drifting or not. A page
	// shorter than the requested size is the last one.
	Scanned int
	// LastID is the highest account id in the page, the cursor for the next.
	LastID uuid.UUID
}

// DriftSweep is the result of one full pass over the accounts table.
type DriftSweep struct {
	Drifts  []BalanceDrift
	Scanned int
	// Truncated reports that the sweep stopped at MaxDriftFindings. Drift is a
	// should-never-happen invariant violation, so reaching this means something
	// systemic is wrong and the individual rows have stopped being the point.
	Truncated bool
}

// Sweep limits.
const (
	// DefaultDriftPageSize is how many accounts one page covers. Small enough
	// that a page's aggregate stays an index scan over a handful of accounts
	// rather than a sequential scan of every entry ever written.
	DefaultDriftPageSize = 500

	// MaxDriftFindings caps what one sweep reports.
	MaxDriftFindings = 10_000
)

// DriftSweepSource is the source name recorded for a sweep's run, so a drift
// report is distinguishable from a statement reconciliation in the same table.
const DriftSweepSource = "balance-drift-sweep"

// SweepDrift walks every account in keyset-paginated pages, comparing each
// cached balance against the sum of that account's entries.
//
// This is a whole-ledger check and it costs a whole-ledger scan; there is no
// predicate that makes it cheaper, because a derived balance is by definition
// the sum of an account's entire history. That is precisely why it runs here,
// as a job, rather than on a request path where an upload of a single CSV row
// would pay for it.
//
// Paging keeps each step bounded and each snapshot short. Accounts in different
// pages are read at different instants, which is harmless: drift is a per-account
// invariant, and one page reads an account's balance and its entries in a single
// statement, so the two sides of the comparison always come from one snapshot.
func SweepDrift(ctx context.Context, scanner DriftScanner, pageSize int) (DriftSweep, error) {
	if pageSize <= 0 {
		pageSize = DefaultDriftPageSize
	}

	var (
		sweep DriftSweep
		after *uuid.UUID
	)
	for {
		page, err := scanner.ScanDriftPage(ctx, after, pageSize)
		if err != nil {
			return sweep, fmt.Errorf("scan drift page after %v: %w", after, err)
		}
		if page.Scanned == 0 {
			return sweep, nil
		}
		// A cursor that does not advance would loop forever. A keyset query
		// cannot produce one, which is the reason to fail loudly here rather
		// than discover it as a job that never ends.
		if after != nil && bytes.Compare(page.LastID[:], after[:]) <= 0 {
			return sweep, fmt.Errorf("drift sweep cursor did not advance past %s", after)
		}

		sweep.Scanned += page.Scanned
		sweep.Drifts = append(sweep.Drifts, page.Drifts...)
		// Strictly greater, so a ledger with exactly the cap's worth of drift is
		// reported in full rather than flagged as truncated when nothing was
		// dropped. The overshoot this allows is one page, which is why the cap
		// is checked here rather than inside the page.
		if len(sweep.Drifts) > MaxDriftFindings {
			sweep.Drifts = sweep.Drifts[:MaxDriftFindings]
			sweep.Truncated = true
			return sweep, nil
		}
		if page.Scanned < pageSize {
			return sweep, nil
		}
		last := page.LastID
		after = &last
	}
}

// DriftRun turns a sweep into a persistable run, so drift is reported through
// the same GET /reconciliation/{id} that serves statement runs.
func DriftRun(sweep DriftSweep) *Run {
	discrepancies := DriftDiscrepancies(sweep.Drifts)
	if sweep.Truncated {
		discrepancies = append(discrepancies, Discrepancy{
			Kind: KindFindingsTruncated,
			Details: map[string]any{
				"reason":       "too many accounts drifted to report individually",
				"reported":     len(sweep.Drifts),
				"max_findings": MaxDriftFindings,
			},
		})
	}
	// The run's row counts are read as accounts here rather than statement
	// lines: examined, and of those, the ones whose cache agreed with their
	// entries. The columns mean "how much was checked" and "how much checked
	// out" either way.
	return &Run{
		ID:               uuid.New(),
		SourceName:       DriftSweepSource,
		StatementRows:    sweep.Scanned,
		MatchedCount:     sweep.Scanned - len(sweep.Drifts),
		DiscrepancyCount: len(discrepancies),
		Discrepancies:    discrepancies,
	}
}
