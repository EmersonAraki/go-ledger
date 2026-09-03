package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// Publisher delivers an envelope to the outside world.
//
// This is the seam where Kafka arrives. Nothing else in the system needs to
// change when it does: the relay, the storage and the replay endpoint all speak
// this interface.
type Publisher interface {
	// Publish delivers the envelope. Returning an error causes the relay to
	// retry with backoff, so implementations must be safe to call more than once
	// for the same event -- delivery is at-least-once, and consumers dedupe on
	// EventID.
	Publish(ctx context.Context, e Envelope) error
}

// LogPublisher writes envelopes to the structured log. It is the default until
// a real broker is wired in, and it makes the outbox observable from day one
// rather than silently doing nothing.
type LogPublisher struct{}

// Publish logs the envelope.
func (LogPublisher) Publish(ctx context.Context, e Envelope) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal envelope %s: %w", e.EventID, err)
	}
	slog.InfoContext(ctx, "outbox publish",
		"event_id", e.EventID,
		"event_type", e.EventType,
		"schema_version", e.SchemaVersion,
		"aggregate_id", e.Aggregate.ID,
		"envelope", json.RawMessage(encoded),
	)
	return nil
}

// RecordingPublisher captures envelopes in memory. It exists for tests, where
// asserting on what would have been published is the whole point.
type RecordingPublisher struct {
	mu        sync.Mutex
	published []Envelope
	// Err, when set, is returned by every Publish call, so a test can exercise
	// the retry and backoff paths.
	Err error
}

// Publish records the envelope, or fails if Err is set.
func (p *RecordingPublisher) Publish(_ context.Context, e Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	p.published = append(p.published, e)
	return nil
}

// Published returns a copy of everything published so far.
func (p *RecordingPublisher) Published() []Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Envelope(nil), p.published...)
}

// Count returns how many envelopes have been published.
func (p *RecordingPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}
