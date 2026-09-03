// Package outbox implements the transactional outbox: every ledger transaction
// writes an event in the same database transaction that writes the entries, so
// an event can never describe a transfer that did not happen, and a transfer can
// never happen without its event.
//
// Publishing is at-least-once. The event id is stable across every delivery and
// every replay, so consumers deduplicate on it.
package outbox

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/ledger"
)

// SchemaVersion is the version of the envelope and payload shapes below.
//
// Compatibility rule, enforced by the golden-file test: within a version fields
// may only be ADDED. Removing a field, renaming one, or changing its type is a
// breaking change and must bump this number, with the relay emitting both
// versions during the migration window.
const SchemaVersion = 1

// Producer identifies this service in every envelope, so a consumer reading a
// shared topic can tell where an event came from.
const Producer = "sumzero"

// Event types. These are contract, not implementation detail: renaming one
// breaks every consumer routing on it.
const (
	EventTypeTransactionCreated = "ledger.transaction.created"
)

// Aggregate types.
const (
	AggregateTransaction = "transaction"
)

// Aggregate identifies what an event is about. Consumers partition on the id to
// get per-aggregate ordering.
type Aggregate struct {
	Type string    `json:"type"`
	ID   uuid.UUID `json:"id"`
}

// Envelope is what a consumer receives. The payload is nested inside so routing
// on event_type and schema_version never requires parsing the body.
type Envelope struct {
	EventID       uuid.UUID       `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Producer      string          `json:"producer"`
	Aggregate     Aggregate       `json:"aggregate"`
	Payload       json.RawMessage `json:"payload"`
}

// EntryPayload is one leg of a transaction as it appears on the wire.
type EntryPayload struct {
	AccountID uuid.UUID `json:"account_id"`
	Direction string    `json:"direction"`
	Amount    int64     `json:"amount"`
}

// TransactionCreatedPayload is the body of a ledger.transaction.created event.
// Amounts are minor units, matching the rest of the API.
type TransactionCreatedPayload struct {
	TransactionID uuid.UUID      `json:"transaction_id"`
	Currency      string         `json:"currency"`
	ExternalRef   *string        `json:"external_ref"`
	Description   string         `json:"description"`
	Entries       []EntryPayload `json:"entries"`
}

// NewTransactionCreated builds the event recorded when a transfer commits.
func NewTransactionCreated(t *ledger.Transaction) (Event, error) {
	entries := make([]EntryPayload, 0, len(t.Entries))
	for _, e := range t.Entries {
		entries = append(entries, EntryPayload{
			AccountID: e.AccountID,
			Direction: string(e.Direction),
			Amount:    e.Amount,
		})
	}

	payload, err := json.Marshal(TransactionCreatedPayload{
		TransactionID: t.ID,
		Currency:      t.Currency,
		ExternalRef:   t.ExternalRef,
		Description:   t.Description,
		Entries:       entries,
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal transaction.created payload: %w", err)
	}

	return Event{
		ID:            uuid.New(),
		AggregateType: AggregateTransaction,
		AggregateID:   t.ID,
		EventType:     EventTypeTransactionCreated,
		SchemaVersion: SchemaVersion,
		Payload:       payload,
		Status:        StatusPending,
	}, nil
}

// Status is the delivery state of a stored event.
type Status string

// Event statuses.
const (
	// StatusPending is waiting to be published, or waiting out a backoff.
	StatusPending Status = "pending"
	// StatusPublished has been handed to the publisher successfully.
	StatusPublished Status = "published"
	// StatusFailed exhausted its attempts. The relay stops retrying it; the
	// replay endpoint exists to put it back in play.
	StatusFailed Status = "failed"
)

// Event is a stored outbox row.
type Event struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	SchemaVersion int
	Payload       json.RawMessage
	Status        Status
	Attempts      int
	LastError     *string
	AvailableAt   time.Time
	PublishedAt   *time.Time
	CreatedAt     time.Time
}

// Envelope assembles the wire form from the stored row. The event id and the
// creation time are the envelope's identity and timestamp, so a replay carries
// exactly the same values as the first delivery.
func (e Event) Envelope() Envelope {
	return Envelope{
		EventID:       e.ID,
		EventType:     e.EventType,
		SchemaVersion: e.SchemaVersion,
		OccurredAt:    e.CreatedAt.UTC(),
		Producer:      Producer,
		Aggregate:     Aggregate{Type: e.AggregateType, ID: e.AggregateID},
		Payload:       e.Payload,
	}
}

// Delivery triggers.
const (
	// TriggerRelay is an automatic attempt by the background relay.
	TriggerRelay = "relay"
	// TriggerManualReplay is an operator-driven attempt via the replay endpoint.
	TriggerManualReplay = "manual_replay"
)

// Delivery is one publish attempt, successful or not. The history is kept so an
// operator can see why an event is stuck.
type Delivery struct {
	ID          int64
	EventID     uuid.UUID
	AttemptedAt time.Time
	Succeeded   bool
	Trigger     string
	Error       *string
}
