package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/outbox"
)

// eventIDFor returns the outbox event id recorded for a transaction.
func (a *testAPI) eventIDFor(transactionID string) uuid.UUID {
	a.t.Helper()

	var id uuid.UUID
	err := a.pool.QueryRow(context.Background(),
		`SELECT id FROM outbox_events WHERE aggregate_id = $1`, transactionID).Scan(&id)
	if err != nil {
		a.t.Fatalf("find event for transaction %s: %v", transactionID, err)
	}
	return id
}

// postTransfer makes a transfer and returns its transaction id.
func (a *testAPI) postTransfer(debit, credit string, amount int64) string {
	a.t.Helper()

	rec := a.do(http.MethodPost, "/transactions", transferBody(debit, credit, amount))
	if rec.Code != http.StatusCreated {
		a.t.Fatalf("transfer: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		ID string `json:"id"`
	}
	a.decode(rec, &resp)
	return resp.ID
}

func TestGetEventReturnsTheEnvelope(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)
	txID := api.postTransfer(bob, alice, 300)

	eventID := api.eventIDFor(txID)

	rec := api.do(http.MethodGet, "/events/"+eventID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var resp struct {
		EventID       string `json:"event_id"`
		EventType     string `json:"event_type"`
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
		Attempts      int    `json:"attempts"`
		Envelope      struct {
			EventID   string `json:"event_id"`
			Producer  string `json:"producer"`
			Aggregate struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"aggregate"`
		} `json:"envelope"`
	}
	api.decode(rec, &resp)

	if resp.EventID != eventID.String() {
		t.Errorf("event_id = %s, want %s", resp.EventID, eventID)
	}
	if resp.EventType != outbox.EventTypeTransactionCreated {
		t.Errorf("event_type = %q", resp.EventType)
	}
	if resp.SchemaVersion != outbox.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", resp.SchemaVersion, outbox.SchemaVersion)
	}
	// No relay runs in these tests, so the event is still awaiting delivery.
	if resp.Status != string(outbox.StatusPending) {
		t.Errorf("status = %q, want pending", resp.Status)
	}
	if resp.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", resp.Attempts)
	}
	if resp.Envelope.Aggregate.ID != txID {
		t.Errorf("envelope aggregate id = %s, want %s", resp.Envelope.Aggregate.ID, txID)
	}
	if resp.Envelope.Aggregate.Type != outbox.AggregateTransaction {
		t.Errorf("envelope aggregate type = %q", resp.Envelope.Aggregate.Type)
	}
}

func TestReplayPublishesAndRecordsTheAttempt(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	alice := api.createAccount("alice", "BRL", false)
	bob := api.createAccount("bob", "BRL", false)
	api.fund(alice, "BRL", 1000)
	txID := api.postTransfer(bob, alice, 300)
	eventID := api.eventIDFor(txID)

	before := api.publisher.Count()

	rec := api.do(http.MethodPost, "/events/"+eventID.String()+"/replay", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}

	var resp struct {
		Event struct {
			EventID string `json:"event_id"`
			Status  string `json:"status"`
		} `json:"event"`
		Delivery struct {
			Succeeded bool   `json:"succeeded"`
			Trigger   string `json:"trigger"`
		} `json:"delivery"`
	}
	api.decode(rec, &resp)

	if !resp.Delivery.Succeeded {
		t.Error("replay reported a failed delivery")
	}
	if resp.Delivery.Trigger != outbox.TriggerManualReplay {
		t.Errorf("trigger = %q, want %q", resp.Delivery.Trigger, outbox.TriggerManualReplay)
	}
	if resp.Event.Status != string(outbox.StatusPublished) {
		t.Errorf("status = %q, want published", resp.Event.Status)
	}

	if got := api.publisher.Count(); got != before+1 {
		t.Fatalf("published %d envelopes, want %d", got, before+1)
	}
	published := api.publisher.Published()[api.publisher.Count()-1]
	if published.EventID != eventID {
		t.Errorf("replay published event id %s, want %s", published.EventID, eventID)
	}

	// Replaying again is allowed -- that is the point of the endpoint -- and the
	// id must stay the same so consumers can deduplicate.
	rec = api.do(http.MethodPost, "/events/"+eventID.String()+"/replay", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second replay: status %d, body %s", rec.Code, rec.Body)
	}
	if api.publisher.Published()[api.publisher.Count()-1].EventID != eventID {
		t.Error("second replay changed the event id")
	}

	// Both attempts show up in the history.
	rec = api.do(http.MethodGet, "/events/"+eventID.String(), nil)
	var withHistory struct {
		Deliveries []struct {
			Succeeded bool   `json:"succeeded"`
			Trigger   string `json:"trigger"`
		} `json:"deliveries"`
	}
	api.decode(rec, &withHistory)
	if len(withHistory.Deliveries) != 2 {
		t.Errorf("got %d deliveries, want 2", len(withHistory.Deliveries))
	}
	for i, d := range withHistory.Deliveries {
		if d.Trigger != outbox.TriggerManualReplay || !d.Succeeded {
			t.Errorf("delivery %d = %+v, want a successful manual_replay", i, d)
		}
	}
}

func TestUnknownEventReturns404(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	missing := uuid.NewString()

	api.assertProblem(api.do(http.MethodGet, "/events/"+missing, nil),
		http.StatusNotFound, "event_not_found")
	api.assertProblem(api.do(http.MethodPost, "/events/"+missing+"/replay", nil),
		http.StatusNotFound, "event_not_found")
}

func TestMalformedEventIDIsBadRequest(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t)

	api.assertProblem(api.do(http.MethodGet, "/events/not-a-uuid", nil),
		http.StatusBadRequest, "invalid_uuid")
	api.assertProblem(api.do(http.MethodPost, "/events/not-a-uuid/replay", nil),
		http.StatusBadRequest, "invalid_uuid")
}
