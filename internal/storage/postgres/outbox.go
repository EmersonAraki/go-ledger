package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/EmersonAraki/go-ledger/internal/outbox"
)

// ErrEventNotFound means no outbox event has the given id.
var ErrEventNotFound = errors.New("event not found")

// insertEvent writes an outbox event on an existing transaction. It is only
// ever called from inside the transfer's transaction, which is the whole point
// of the pattern: the event and the entries commit together or not at all.
func insertEvent(ctx context.Context, tx pgx.Tx, e *outbox.Event) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO outbox_events
		    (id, aggregate_type, aggregate_id, event_type, schema_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, available_at, status`,
		e.ID, e.AggregateType, e.AggregateID, e.EventType, e.SchemaVersion, e.Payload,
	).Scan(&e.CreatedAt, &e.AvailableAt, &e.Status)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// eventColumns is the column list shared by every event read, so the scan order
// cannot drift between queries.
const eventColumns = `id, aggregate_type, aggregate_id, event_type, schema_version,
	payload, status, attempts, last_error, available_at, published_at, created_at`

func scanEvent(row pgx.Row) (*outbox.Event, error) {
	var e outbox.Event
	err := row.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType, &e.SchemaVersion,
		&e.Payload, &e.Status, &e.Attempts, &e.LastError, &e.AvailableAt, &e.PublishedAt, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// GetEvent loads one outbox event.
func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (*outbox.Event, error) {
	e, err := scanEvent(s.pool.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM outbox_events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrEventNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("select outbox event: %w", err)
	}
	return e, nil
}

// Deliveries returns an event's attempt history, newest last.
func (s *Store) Deliveries(ctx context.Context, eventID uuid.UUID) ([]outbox.Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, event_id, attempted_at, succeeded, trigger, error
		  FROM outbox_deliveries
		 WHERE event_id = $1
		 ORDER BY id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("select deliveries: %w", err)
	}
	defer rows.Close()

	var out []outbox.Delivery
	for rows.Next() {
		var d outbox.Delivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.AttemptedAt,
			&d.Succeeded, &d.Trigger, &d.Error); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DispatchBatch claims up to limit due events, publishes each, and records the
// outcome -- all in one transaction.
//
// FOR UPDATE SKIP LOCKED is what lets several relay instances run with no
// coordination: each skips rows another instance already holds instead of
// queueing behind them, so throughput scales with instances and no event is
// delivered twice concurrently.
//
// Publishing happens inside the transaction. If the publish succeeds but the
// commit does not, the event stays pending and is published again later. That
// is at-least-once by construction, which is why consumers dedupe on event id;
// the alternative -- committing before publishing -- would be at-most-once and
// could silently lose events.
func (s *Store) DispatchBatch(ctx context.Context, limit int, p outbox.Publisher) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin dispatch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, `
		SELECT `+eventColumns+`
		  FROM outbox_events
		 -- Only 'pending'. A 'failed' event has exhausted its attempts and is
		 -- deliberately left alone: retrying it forever would burn relay
		 -- capacity on something that needs a human. ReplayEvent is how it comes
		 -- back. The partial index (status <> 'published') still covers this,
		 -- since pending implies not published.
		 WHERE status = 'pending' AND available_at <= now()
		 ORDER BY created_at
		   FOR UPDATE SKIP LOCKED
		 LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("claim outbox batch: %w", err)
	}

	var batch []*outbox.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		batch = append(batch, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read outbox batch: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	for _, e := range batch {
		publishErr := p.Publish(ctx, e.Envelope())
		if err := settleAttempt(ctx, tx, e, publishErr, outbox.TriggerRelay); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit dispatch: %w", err)
	}
	return len(batch), nil
}

// settleAttempt records one publish attempt and moves the event to its next
// state: published on success, or backed off -- and eventually failed -- on
// error.
func settleAttempt(ctx context.Context, tx pgx.Tx, e *outbox.Event, publishErr error, trigger string) error {
	attempts := e.Attempts + 1

	if publishErr == nil {
		_, err := tx.Exec(ctx, `
			UPDATE outbox_events
			   SET status = 'published', published_at = now(), attempts = $2, last_error = NULL
			 WHERE id = $1`, e.ID, attempts)
		if err != nil {
			return fmt.Errorf("mark event published: %w", err)
		}
		return recordDelivery(ctx, tx, e.ID, true, trigger, nil)
	}

	// Give up after MaxAttempts so a permanently undeliverable event stops
	// consuming relay capacity. The replay endpoint is how it comes back.
	status := outbox.StatusPending
	if attempts >= outbox.MaxAttempts {
		status = outbox.StatusFailed
	}
	msg := publishErr.Error()

	_, err := tx.Exec(ctx, `
		UPDATE outbox_events
		   SET status = $2, attempts = $3, last_error = $4, available_at = now() + $5::interval
		 WHERE id = $1`,
		e.ID, status, attempts, msg, intervalOf(outbox.Backoff(attempts)))
	if err != nil {
		return fmt.Errorf("mark event failed: %w", err)
	}
	return recordDelivery(ctx, tx, e.ID, false, trigger, &msg)
}

// intervalOf renders a duration for PostgreSQL's interval parser.
func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}

func recordDelivery(ctx context.Context, tx pgx.Tx, eventID uuid.UUID,
	succeeded bool, trigger string, errMsg *string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_deliveries (event_id, succeeded, trigger, error)
		VALUES ($1, $2, $3, $4)`, eventID, succeeded, trigger, errMsg)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// ReplayEvent re-publishes an event on demand, whatever its current status.
//
// The event id is deliberately unchanged: a replay is another delivery of the
// same event, not a new one, so consumers deduplicating on event id will ignore
// it if they already handled it. That is exactly what makes replay safe to run.
func (s *Store) ReplayEvent(ctx context.Context, id uuid.UUID, p outbox.Publisher) (*outbox.Event, *outbox.Delivery, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin replay: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Lock the row so a concurrent relay tick cannot publish the same event at
	// the same moment.
	e, err := scanEvent(tx.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM outbox_events WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: %s", ErrEventNotFound, id)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("select event for replay: %w", err)
	}

	publishErr := p.Publish(ctx, e.Envelope())
	if err := settleAttempt(ctx, tx, e, publishErr, outbox.TriggerManualReplay); err != nil {
		return nil, nil, err
	}

	// Read back the settled state and the delivery just recorded, so the caller
	// reports what actually happened rather than what was intended.
	settled, err := scanEvent(tx.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM outbox_events WHERE id = $1`, id))
	if err != nil {
		return nil, nil, fmt.Errorf("reload replayed event: %w", err)
	}

	var d outbox.Delivery
	err = tx.QueryRow(ctx, `
		SELECT id, event_id, attempted_at, succeeded, trigger, error
		  FROM outbox_deliveries
		 WHERE event_id = $1
		 ORDER BY id DESC
		 LIMIT 1`, id,
	).Scan(&d.ID, &d.EventID, &d.AttemptedAt, &d.Succeeded, &d.Trigger, &d.Error)
	if err != nil {
		return nil, nil, fmt.Errorf("read replay delivery: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit replay: %w", err)
	}
	return settled, &d, nil
}
