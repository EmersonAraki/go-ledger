package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Relay drives publishing. It owns the schedule; the Dispatcher owns the
// database transaction that claims and settles each batch.
type Relay struct {
	dispatcher Dispatcher
	publisher  Publisher
	interval   time.Duration
	batchSize  int
}

// Dispatcher claims a batch of due events, publishes each and records the
// outcome, all inside one database transaction.
//
// The claim and the settlement share a transaction so a relay that dies
// mid-batch releases its locks and leaves the events pending for another
// instance, rather than stranding them in a half-processed state.
type Dispatcher interface {
	DispatchBatch(ctx context.Context, limit int, p Publisher) (int, error)
}

// RelayOptions configures a relay. Zero values fall back to the defaults.
type RelayOptions struct {
	Interval  time.Duration
	BatchSize int
}

// Relay defaults.
const (
	DefaultInterval  = 500 * time.Millisecond
	DefaultBatchSize = 100
)

// NewRelay builds a relay over a dispatcher and publisher.
func NewRelay(d Dispatcher, p Publisher, opts RelayOptions) *Relay {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	return &Relay{dispatcher: d, publisher: p, interval: opts.Interval, batchSize: opts.BatchSize}
}

// Run polls until the context is cancelled.
//
// A batch error is logged and retried on the next tick rather than killing the
// loop: a transient database problem must not stop event delivery permanently.
// Returns nil on a clean shutdown.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	slog.InfoContext(ctx, "outbox relay started",
		"interval", r.interval, "batch_size", r.batchSize)

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "outbox relay stopped")
			return nil
		case <-ticker.C:
			// Drain: keep going while batches come back full, so a backlog is
			// cleared promptly instead of one batch per tick.
			for {
				n, err := r.dispatcher.DispatchBatch(ctx, r.batchSize, r.publisher)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						break
					}
					slog.ErrorContext(ctx, "outbox batch failed", "error", err)
					break
				}
				if n < r.batchSize {
					break
				}
			}
		}
	}
}

// Retry policy.
const (
	// MaxAttempts is how many times the relay will try before giving up and
	// marking an event failed. Giving up is deliberate: an event that cannot be
	// delivered should stop consuming capacity and wait for a human, which is
	// what the replay endpoint is for.
	MaxAttempts = 10

	// MaxBackoff caps the exponential delay between attempts.
	MaxBackoff = 5 * time.Minute
)

// Backoff returns how long to wait before the next attempt, growing
// exponentially from one second and capped at MaxBackoff.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// Cap the shift before it can overflow.
	if attempts > 20 {
		return MaxBackoff
	}
	d := time.Duration(1<<uint(attempts-1)) * time.Second
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}
