package outbox_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/ledger"
	"github.com/EmersonAraki/go-ledger/internal/outbox"
)

var update = flag.Bool("update", false, "rewrite the golden envelope file")

// TestEnvelopeGolden pins the wire contract.
//
// Consumers of this event live in other services and cannot be changed in
// lockstep, so the envelope's shape is an API. Within a schema version fields
// may only be ADDED; if this test fails because a field was renamed, retyped or
// removed, the correct response is to bump SchemaVersion, not to update the
// golden file.
func TestEnvelopeGolden(t *testing.T) {
	t.Parallel()

	// Fixed ids and timestamp so the output is deterministic.
	txID := uuid.MustParse("0192f3a0-1111-7000-8000-000000000001")
	eventID := uuid.MustParse("0192f3a1-2222-7000-8000-000000000002")
	debit := uuid.MustParse("0192f3a2-3333-7000-8000-000000000003")
	credit := uuid.MustParse("0192f3a3-4444-7000-8000-000000000004")
	ref := "TRX-88231"

	event, err := outbox.NewTransactionCreated(&ledger.Transaction{
		ID:          txID,
		ExternalRef: &ref,
		Description: "rent",
		Currency:    "BRL",
		Entries: []ledger.Entry{
			{AccountID: debit, Direction: ledger.Debit, Amount: 12345, Currency: "BRL"},
			{AccountID: credit, Direction: ledger.Credit, Amount: 12345, Currency: "BRL"},
		},
	})
	if err != nil {
		t.Fatalf("build event: %v", err)
	}

	// NewTransactionCreated generates a random id and the database stamps
	// created_at; pin both so only the shape is under test.
	event.ID = eventID
	event.CreatedAt = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	got, err := json.MarshalIndent(event.Envelope(), "", "  ")
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "transaction_created_v1.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("envelope shape changed.\n--- want ---\n%s\n--- got ---\n%s\n"+
			"If a field was added, run: go test ./internal/outbox -update\n"+
			"If a field was renamed, retyped or removed, bump SchemaVersion instead.",
			want, got)
	}
}

// The event id and occurred-at are the envelope's identity. A replay must carry
// the same ones, or consumer deduplication cannot work.
func TestEnvelopeIdentityIsStable(t *testing.T) {
	t.Parallel()

	e := outbox.Event{
		ID:            uuid.New(),
		AggregateType: outbox.AggregateTransaction,
		AggregateID:   uuid.New(),
		EventType:     outbox.EventTypeTransactionCreated,
		SchemaVersion: outbox.SchemaVersion,
		Payload:       json.RawMessage(`{"a":1}`),
		CreatedAt:     time.Now(),
	}

	first, second := e.Envelope(), e.Envelope()
	if first.EventID != e.ID || second.EventID != e.ID {
		t.Error("envelope event id does not match the stored event id")
	}
	if !first.OccurredAt.Equal(second.OccurredAt) {
		t.Error("occurred_at is not stable across envelope construction")
	}
	if first.Producer != outbox.Producer {
		t.Errorf("producer = %q, want %q", first.Producer, outbox.Producer)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	if got := outbox.Backoff(1); got != time.Second {
		t.Errorf("Backoff(1) = %v, want 1s", got)
	}
	if got := outbox.Backoff(2); got != 2*time.Second {
		t.Errorf("Backoff(2) = %v, want 2s", got)
	}

	// Must grow monotonically and never exceed the cap, including at absurd
	// attempt counts where a naive shift would overflow.
	prev := time.Duration(0)
	for attempts := 1; attempts <= 64; attempts++ {
		got := outbox.Backoff(attempts)
		if got < prev {
			t.Errorf("Backoff(%d) = %v went backwards from %v", attempts, got, prev)
		}
		if got > outbox.MaxBackoff {
			t.Errorf("Backoff(%d) = %v exceeds the %v cap", attempts, got, outbox.MaxBackoff)
		}
		if got <= 0 {
			t.Errorf("Backoff(%d) = %v, must be positive", attempts, got)
		}
		prev = got
	}
}
