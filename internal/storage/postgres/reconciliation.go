package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

// ErrRunNotFound means no reconciliation run has the given id.
var ErrRunNotFound = errors.New("reconciliation run not found")

// Reconcile compares a parsed statement against the ledger and stores the result.
//
// The comparison runs in a REPEATABLE READ, read-only transaction. This is the
// one place in the system where snapshot isolation is exactly the right tool:
// the job issues several queries -- the transactions in the window, then the
// balance-drift check across every account -- and they must all observe the same
// instant. Under READ COMMITTED a transfer committing between those queries
// would appear in one and not the other, and the job would invent a discrepancy
// that never existed.
//
// Note the contrast with the write path, which deliberately does NOT use
// REPEATABLE READ: there, snapshot isolation fails to prevent write skew. Same
// isolation level, opposite verdict, because the requirements differ.
func (s *Store) Reconcile(
	ctx context.Context,
	sourceName string,
	rows []reconcile.StatementRow,
	parseErrors []reconcile.Discrepancy,
	opts reconcile.Options,
) (*reconcile.Run, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin reconciliation snapshot: %w", err)
	}
	// Read-only, so this only releases the snapshot.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	start, end := reconcile.Window(rows)
	ledger, err := loadLedgerWindow(ctx, tx, start, end, opts.DateTolerance)
	if err != nil {
		return nil, err
	}

	matched, discrepancies := reconcile.Match(rows, ledger, opts)

	drifts, err := loadBalanceDrift(ctx, tx)
	if err != nil {
		return nil, err
	}

	// Rollback rather than commit: nothing was written, and the snapshot has
	// served its purpose.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return nil, fmt.Errorf("release reconciliation snapshot: %w", err)
	}

	all := make([]reconcile.Discrepancy, 0, len(parseErrors)+len(discrepancies)+len(drifts))
	all = append(all, parseErrors...)
	all = append(all, discrepancies...)
	all = append(all, reconcile.DriftDiscrepancies(drifts)...)

	run := &reconcile.Run{
		ID:               uuid.New(),
		SourceName:       sourceName,
		StatementRows:    len(rows) + len(parseErrors),
		MatchedCount:     matched,
		DiscrepancyCount: len(all),
		WindowStart:      start,
		WindowEnd:        end,
		Discrepancies:    all,
	}

	if err := s.saveRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// loadLedgerWindow reads the transactions the statement could plausibly be
// talking about. The window is widened by the date tolerance so a transfer
// posted just outside the statement's range can still be matched to a row
// inside it, rather than being reported missing on both sides.
func loadLedgerWindow(ctx context.Context, tx pgx.Tx, start, end *time.Time,
	tolerance time.Duration) ([]reconcile.LedgerTransaction, error) {
	if start == nil || end == nil {
		return nil, nil
	}
	if tolerance <= 0 {
		tolerance = reconcile.DefaultDateTolerance
	}

	// One row per transaction, with its two legs folded in. Restricting to
	// exactly two entries keeps the shape the statement format can express;
	// a multi-leg transaction could not be represented on one statement line
	// anyway, and silently flattening one would misreport it.
	rows, err := tx.Query(ctx, `
		SELECT t.id,
		       t.external_ref,
		       t.created_at,
		       d.account_id AS debit_account_id,
		       c.account_id AS credit_account_id,
		       d.amount,
		       t.currency
		  FROM transactions t
		  JOIN ledger_entries d ON d.transaction_id = t.id AND d.direction = 'debit'
		  JOIN ledger_entries c ON c.transaction_id = t.id AND c.direction = 'credit'
		 WHERE t.created_at BETWEEN $1::timestamptz - $3::interval
		                        AND $2::timestamptz + $3::interval
		   AND (SELECT COUNT(*) FROM ledger_entries e WHERE e.transaction_id = t.id) = 2
		 ORDER BY t.created_at`,
		start.UTC(), end.UTC(), intervalOf(tolerance))
	if err != nil {
		return nil, fmt.Errorf("load ledger window: %w", err)
	}
	defer rows.Close()

	var out []reconcile.LedgerTransaction
	for rows.Next() {
		var t reconcile.LedgerTransaction
		if err := rows.Scan(&t.ID, &t.ExternalRef, &t.PostedAt,
			&t.DebitAccountID, &t.CreditAccountID, &t.Amount, &t.Currency); err != nil {
			return nil, fmt.Errorf("scan ledger transaction: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// loadBalanceDrift finds accounts whose cached balance disagrees with the sum of
// their entries. This is what keeps the materialized balance honest: the cache
// is a performance choice, and SUM(signed_amount) remains the source of truth.
func loadBalanceDrift(ctx context.Context, tx pgx.Tx) ([]reconcile.BalanceDrift, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name, a.balance, COALESCE(SUM(e.signed_amount), 0) AS derived
		  FROM accounts a
		  LEFT JOIN ledger_entries e ON e.account_id = a.id
		 GROUP BY a.id, a.name, a.balance
		HAVING a.balance <> COALESCE(SUM(e.signed_amount), 0)
		 ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("check balance drift: %w", err)
	}
	defer rows.Close()

	var out []reconcile.BalanceDrift
	for rows.Next() {
		var d reconcile.BalanceDrift
		if err := rows.Scan(&d.AccountID, &d.Name, &d.Cached, &d.Derived); err != nil {
			return nil, fmt.Errorf("scan balance drift: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// saveRun persists the run and its discrepancies in one transaction, so a report
// is never half-written.
func (s *Store) saveRun(ctx context.Context, run *reconcile.Run) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin save run: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO reconciliation_runs
		    (id, source_name, statement_rows, matched_count, discrepancy_count,
		     window_start, window_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at`,
		run.ID, run.SourceName, run.StatementRows, run.MatchedCount,
		run.DiscrepancyCount, run.WindowStart, run.WindowEnd,
	).Scan(&run.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert reconciliation run: %w", err)
	}

	for i, d := range run.Discrepancies {
		details, err := json.Marshal(d.Details)
		if err != nil {
			return fmt.Errorf("marshal discrepancy %d details: %w", i, err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO reconciliation_discrepancies
			    (run_id, kind, statement_ref, transaction_id, details)
			VALUES ($1, $2, $3, $4, $5)`,
			run.ID, d.Kind, d.StatementRef, d.TransactionID, details)
		if err != nil {
			return fmt.Errorf("insert discrepancy %d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reconciliation run: %w", err)
	}
	return nil
}

// GetRun loads a run's summary.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (*reconcile.Run, error) {
	var run reconcile.Run
	err := s.pool.QueryRow(ctx, `
		SELECT id, source_name, statement_rows, matched_count, discrepancy_count,
		       window_start, window_end, created_at
		  FROM reconciliation_runs WHERE id = $1`, id,
	).Scan(&run.ID, &run.SourceName, &run.StatementRows, &run.MatchedCount,
		&run.DiscrepancyCount, &run.WindowStart, &run.WindowEnd, &run.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("select reconciliation run: %w", err)
	}
	return &run, nil
}

// ListDiscrepancies returns a page of a run's findings, ordered stably by id.
// Pagination is keyset-based: `after` is the last id from the previous page.
//
// The returned bool reports whether more pages follow. It is derived by asking
// for one row beyond the page and discarding it, rather than by guessing from a
// full page -- otherwise a run whose findings divide exactly by the page size
// hands the client a cursor that leads to an empty page.
func (s *Store) ListDiscrepancies(ctx context.Context, runID uuid.UUID,
	after int64, limit int) ([]reconcile.Discrepancy, int64, bool, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, statement_ref, transaction_id, details
		  FROM reconciliation_discrepancies
		 WHERE run_id = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`, runID, after, limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("select discrepancies: %w", err)
	}
	defer rows.Close()

	var (
		out []reconcile.Discrepancy
		ids []int64
	)
	for rows.Next() {
		var (
			id      int64
			d       reconcile.Discrepancy
			details []byte
		)
		if err := rows.Scan(&id, &d.Kind, &d.StatementRef, &d.TransactionID, &details); err != nil {
			return nil, 0, false, fmt.Errorf("scan discrepancy: %w", err)
		}
		if err := json.Unmarshal(details, &d.Details); err != nil {
			return nil, 0, false, fmt.Errorf("decode discrepancy details: %w", err)
		}
		out = append(out, d)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("read discrepancies: %w", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
		ids = ids[:limit]
	}

	var last int64
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	return out, last, hasMore, nil
}
