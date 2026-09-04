package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

// ScanDriftPage reads one keyset page of accounts and compares each cached
// balance against the sum of that account's entries.
//
// The sum is a correlated subquery per account, deliberately, and not a join
// against the page. Measured over 400k entries, the join plan seq-scanned every
// entry in the table for a page of ten accounts -- 49ms a page, so paging made
// the sweep worse the smaller the pages got. Per account, the planner uses
// ledger_entries_account_id_created_at_idx instead: the same page costs 0.8ms,
// and the sweep's cost finally scales with the page rather than the ledger.
//
// Balance and sum come from one statement, hence one snapshot, so a transfer
// committing mid-sweep cannot be misreported as drift.
func (s *Store) ScanDriftPage(ctx context.Context, after uuid.UUID, limit int) (reconcile.DriftPage, error) {
	if limit <= 0 {
		limit = reconcile.DefaultDriftPageSize
	}

	rows, err := s.pool.Query(ctx, `
		SELECT p.id,
		       p.name,
		       p.balance,
		       COALESCE((SELECT SUM(e.signed_amount)
		                   FROM ledger_entries e
		                  WHERE e.account_id = p.id), 0) AS derived
		  FROM (SELECT id, name, balance
		          FROM accounts
		         WHERE id > $1
		         ORDER BY id
		         LIMIT $2) p
		 ORDER BY p.id`, after, limit)
	if err != nil {
		return reconcile.DriftPage{}, fmt.Errorf("scan drift page: %w", err)
	}
	defer rows.Close()

	var page reconcile.DriftPage
	for rows.Next() {
		var d reconcile.BalanceDrift
		if err := rows.Scan(&d.AccountID, &d.Name, &d.Cached, &d.Derived); err != nil {
			return reconcile.DriftPage{}, fmt.Errorf("scan drift row: %w", err)
		}
		page.Scanned++
		// Ordered by id, so the last row read carries the cursor -- including
		// when it did not drift, which is the normal case.
		page.LastID = d.AccountID
		if d.Cached != d.Derived {
			page.Drifts = append(page.Drifts, d)
		}
	}
	if err := rows.Err(); err != nil {
		return reconcile.DriftPage{}, fmt.Errorf("read drift page: %w", err)
	}
	return page, nil
}

// SweepBalanceDrift runs a full drift sweep and records a run when it finds
// anything, returning nil when every account's cache agreed with its entries.
//
// Nothing is persisted for a clean sweep: drift is an invariant violation, so a
// run row means something is wrong, and the absence of one is the normal state
// rather than an absence of evidence.
func (s *Store) SweepBalanceDrift(ctx context.Context, pageSize int) (*reconcile.Run, error) {
	sweep, err := reconcile.SweepDrift(ctx, s, pageSize)
	if err != nil {
		return nil, err
	}
	if len(sweep.Drifts) == 0 {
		return nil, nil
	}

	run := reconcile.DriftRun(sweep)
	if err := s.saveRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}
