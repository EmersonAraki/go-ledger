package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/ledger"
	"github.com/EmersonAraki/go-ledger/internal/outbox"
	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// outboxFixture is a store with funded accounts ready to transfer between.
type outboxFixture struct {
	pool   *pgxpool.Pool
	store  *postgres.Store
	alice  uuid.UUID
	bob    uuid.UUID
	source uuid.UUID
}

func newOutboxFixture(ctx context.Context, t *testing.T) *outboxFixture {
	t.Helper()

	pool := pgtest.Pool(t)
	f := &outboxFixture{
		pool:   pool,
		store:  postgres.NewStore(pool),
		alice:  newAccount(ctx, t, pool, "alice", 0, false),
		bob:    newAccount(ctx, t, pool, "bob", 0, false),
		source: newAccount(ctx, t, pool, "external", 0, true),
	}
	// Fund alice from the system account so later transfers have money to move.
	f.transfer(ctx, t, f.alice, f.source, 100_000)
	return f
}

// transfer posts a transfer through the store, which is what writes the outbox
// event in the same transaction.
func (f *outboxFixture) transfer(ctx context.Context, t *testing.T, debit, credit uuid.UUID, amount int64) *ledger.Transaction {
	t.Helper()

	claim := ledger.Claim{
		Key:         uuid.NewString(),
		Endpoint:    "POST /transactions",
		Fingerprint: []byte(uuid.NewString()),
	}
	render := func(tx *ledger.Transaction) (int, []byte, error) {
		b, err := json.Marshal(map[string]string{"id": tx.ID.String()})
		return http.StatusCreated, b, err
	}

	result, err := f.store.Transfer(ctx, ledger.TransferCommand{
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          amount,
		Currency:        "BRL",
	}, claim, render)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	return result.Transaction
}

func (f *outboxFixture) countEvents(ctx context.Context, t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// A committed transfer must leave exactly one event behind -- no more, and
// never none.
func TestTransferWritesExactlyOneOutboxEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	before := f.countEvents(ctx, t)
	tx := f.transfer(ctx, t, f.bob, f.alice, 500)

	if got := f.countEvents(ctx, t) - before; got != 1 {
		t.Fatalf("transfer produced %d events, want exactly 1", got)
	}

	var (
		aggregateID uuid.UUID
		eventType   string
		version     int
		status      string
		payload     json.RawMessage
	)
	err := f.pool.QueryRow(ctx, `
		SELECT aggregate_id, event_type, schema_version, status, payload
		  FROM outbox_events WHERE aggregate_id = $1`, tx.ID,
	).Scan(&aggregateID, &eventType, &version, &status, &payload)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}

	if aggregateID != tx.ID {
		t.Errorf("aggregate_id = %s, want %s", aggregateID, tx.ID)
	}
	if eventType != outbox.EventTypeTransactionCreated {
		t.Errorf("event_type = %q, want %q", eventType, outbox.EventTypeTransactionCreated)
	}
	if version != outbox.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", version, outbox.SchemaVersion)
	}
	if status != string(outbox.StatusPending) {
		t.Errorf("status = %q, want pending", status)
	}

	var body outbox.TransactionCreatedPayload
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Errorf("payload has %d entries, want 2", len(body.Entries))
	}
}

// The event and the money share a transaction, so a rejected transfer must
// leave no event at all. This is the guarantee the pattern exists for.
func TestRejectedTransferWritesNoOutboxEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	before := f.countEvents(ctx, t)

	// bob has no funds, so this fails after the event would have been written.
	claim := ledger.Claim{Key: uuid.NewString(), Endpoint: "POST /transactions", Fingerprint: []byte("x")}
	render := func(*ledger.Transaction) (int, []byte, error) { return 201, []byte(`{}`), nil }
	_, err := f.store.Transfer(ctx, ledger.TransferCommand{
		DebitAccountID: f.alice, CreditAccountID: f.bob, Amount: 999_999, Currency: "BRL",
	}, claim, render)
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}

	if got := f.countEvents(ctx, t) - before; got != 0 {
		t.Errorf("a rejected transfer left %d events behind, want 0", got)
	}
}

func TestDispatchPublishesPendingEventsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	f.transfer(ctx, t, f.bob, f.alice, 100)
	f.transfer(ctx, t, f.bob, f.alice, 200)
	total := f.countEvents(ctx, t) // includes the funding transfer

	pub := &outbox.RecordingPublisher{}
	n, err := f.store.DispatchBatch(ctx, 10, pub)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if n != total {
		t.Errorf("dispatched %d, want %d", n, total)
	}
	if pub.Count() != total {
		t.Errorf("published %d, want %d", pub.Count(), total)
	}

	// A second pass must find nothing: published events are not re-delivered.
	n, err = f.store.DispatchBatch(ctx, 10, pub)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if n != 0 {
		t.Errorf("second dispatch handled %d events, want 0", n)
	}
	if pub.Count() != total {
		t.Errorf("published %d after second pass, want %d", pub.Count(), total)
	}

	for _, env := range pub.Published() {
		if env.SchemaVersion != outbox.SchemaVersion {
			t.Errorf("envelope schema_version = %d", env.SchemaVersion)
		}
		if env.Producer != outbox.Producer {
			t.Errorf("envelope producer = %q", env.Producer)
		}
	}
}

// Two relays running at once must not deliver the same event twice. This is
// what FOR UPDATE SKIP LOCKED buys: each instance takes a disjoint batch.
func TestConcurrentRelaysDoNotDoublePublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	const transfers = 20
	for range transfers {
		f.transfer(ctx, t, f.bob, f.alice, 10)
	}
	total := f.countEvents(ctx, t)

	// One shared publisher: every delivery from either relay lands in it, so a
	// duplicate would show up as a repeated event id.
	pub := &outbox.RecordingPublisher{}

	const relays = 4
	var wg sync.WaitGroup
	errs := make([]error, relays)
	for i := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Loop until there is nothing left, as a real relay would.
			for {
				n, err := f.store.DispatchBatch(ctx, 3, pub)
				if err != nil {
					errs[i] = err
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("relay %d: %v", i, err)
		}
	}

	seen := map[uuid.UUID]int{}
	for _, env := range pub.Published() {
		seen[env.EventID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("event %s published %d times, want exactly 1", id, count)
		}
	}
	if len(seen) != total {
		t.Errorf("published %d distinct events, want %d", len(seen), total)
	}

	var unpublished int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE status <> 'published'`).Scan(&unpublished); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	if unpublished != 0 {
		t.Errorf("%d events left unpublished", unpublished)
	}

	// Exactly one successful delivery row per event.
	var deliveries int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_deliveries WHERE succeeded`).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != total {
		t.Errorf("%d successful deliveries recorded, want %d", deliveries, total)
	}
}

func TestFailedPublishBacksOffThenGivesUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	failing := &outbox.RecordingPublisher{Err: errors.New("broker unavailable")}

	// First failure: still pending, one attempt, error recorded, retry deferred.
	if _, err := f.store.DispatchBatch(ctx, 10, failing); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var (
		status    string
		attempts  int
		lastError *string
		deferred  bool
	)
	err := f.pool.QueryRow(ctx, `
		SELECT status, attempts, last_error, available_at > now()
		  FROM outbox_events LIMIT 1`).Scan(&status, &attempts, &lastError, &deferred)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if status != string(outbox.StatusPending) {
		t.Errorf("status = %q, want pending after one failure", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == nil || *lastError == "" {
		t.Error("last_error was not recorded")
	}
	if !deferred {
		t.Error("available_at was not pushed into the future; backoff is not being applied")
	}

	// A due event is skipped while backing off.
	n, err := f.store.DispatchBatch(ctx, 10, failing)
	if err != nil {
		t.Fatalf("dispatch during backoff: %v", err)
	}
	if n != 0 {
		t.Errorf("dispatched %d events during backoff, want 0", n)
	}

	// Drive it to the attempt limit, clearing the backoff each round the way
	// time passing would.
	for range outbox.MaxAttempts {
		if _, err := f.pool.Exec(ctx, `UPDATE outbox_events SET available_at = now()`); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		if _, err := f.store.DispatchBatch(ctx, 10, failing); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}

	if err := f.pool.QueryRow(ctx,
		`SELECT status, attempts FROM outbox_events LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if status != string(outbox.StatusFailed) {
		t.Errorf("status = %q, want failed after %d attempts", status, outbox.MaxAttempts)
	}
	if attempts < outbox.MaxAttempts {
		t.Errorf("attempts = %d, want at least %d", attempts, outbox.MaxAttempts)
	}

	// A failed event is no longer retried automatically -- that is what replay
	// is for.
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_events SET available_at = now()`); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	working := &outbox.RecordingPublisher{}
	if _, err := f.store.DispatchBatch(ctx, 10, working); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if working.Count() != 0 {
		t.Errorf("a failed event was picked up automatically; %d published", working.Count())
	}
}

func TestReplayRepublishesWithTheSameEventID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	pub := &outbox.RecordingPublisher{}
	if _, err := f.store.DispatchBatch(ctx, 10, pub); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if pub.Count() == 0 {
		t.Fatal("nothing was published to replay")
	}
	original := pub.Published()[0]

	event, delivery, err := f.store.ReplayEvent(ctx, original.EventID, pub)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !delivery.Succeeded {
		t.Errorf("replay delivery failed: %+v", delivery.Error)
	}
	if delivery.Trigger != outbox.TriggerManualReplay {
		t.Errorf("trigger = %q, want %q", delivery.Trigger, outbox.TriggerManualReplay)
	}
	if event.Status != outbox.StatusPublished {
		t.Errorf("status = %q, want published", event.Status)
	}

	replayed := pub.Published()[pub.Count()-1]
	if replayed.EventID != original.EventID {
		t.Errorf("replay changed the event id: %s -> %s", original.EventID, replayed.EventID)
	}
	if !replayed.OccurredAt.Equal(original.OccurredAt) {
		t.Errorf("replay changed occurred_at: %s -> %s", original.OccurredAt, replayed.OccurredAt)
	}

	// The history records both attempts.
	deliveries, err := f.store.Deliveries(ctx, original.EventID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("got %d deliveries, want 2 (relay then replay)", len(deliveries))
	}
	if deliveries[0].Trigger != outbox.TriggerRelay {
		t.Errorf("first delivery trigger = %q, want %q", deliveries[0].Trigger, outbox.TriggerRelay)
	}
}

// Replay is how a failed event comes back into play.
func TestReplayRecoversAFailedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	failing := &outbox.RecordingPublisher{Err: fmt.Errorf("broker down")}
	for range outbox.MaxAttempts {
		if _, err := f.pool.Exec(ctx, `UPDATE outbox_events SET available_at = now()`); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		if _, err := f.store.DispatchBatch(ctx, 10, failing); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}

	var id uuid.UUID
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT id, status FROM outbox_events LIMIT 1`).Scan(&id, &status); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if status != string(outbox.StatusFailed) {
		t.Fatalf("setup: status = %q, want failed", status)
	}

	working := &outbox.RecordingPublisher{}
	event, delivery, err := f.store.ReplayEvent(ctx, id, working)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !delivery.Succeeded {
		t.Error("replay of a failed event did not succeed")
	}
	if event.Status != outbox.StatusPublished {
		t.Errorf("status = %q, want published after a successful replay", event.Status)
	}
	if working.Count() != 1 {
		t.Errorf("published %d, want 1", working.Count())
	}
}

func TestReplayUnknownEventIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(ctx, t)

	_, _, err := f.store.ReplayEvent(ctx, uuid.New(), &outbox.RecordingPublisher{})
	if !errors.Is(err, postgres.ErrEventNotFound) {
		t.Errorf("got %v, want ErrEventNotFound", err)
	}

	if _, err := f.store.GetEvent(ctx, uuid.New()); !errors.Is(err, postgres.ErrEventNotFound) {
		t.Errorf("GetEvent: got %v, want ErrEventNotFound", err)
	}
}
