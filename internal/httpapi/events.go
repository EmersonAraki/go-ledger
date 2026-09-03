package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
	"github.com/EmersonAraki/go-ledger/internal/outbox"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

type deliveryResponse struct {
	AttemptedAt string  `json:"attempted_at"`
	Succeeded   bool    `json:"succeeded"`
	Trigger     string  `json:"trigger"`
	Error       *string `json:"error"`
}

func newDeliveryResponse(d outbox.Delivery) deliveryResponse {
	return deliveryResponse{
		AttemptedAt: d.AttemptedAt.UTC().Format(timeFormat),
		Succeeded:   d.Succeeded,
		Trigger:     d.Trigger,
		Error:       d.Error,
	}
}

type eventResponse struct {
	EventID       uuid.UUID          `json:"event_id"`
	EventType     string             `json:"event_type"`
	SchemaVersion int                `json:"schema_version"`
	Status        string             `json:"status"`
	Attempts      int                `json:"attempts"`
	LastError     *string            `json:"last_error"`
	CreatedAt     string             `json:"created_at"`
	PublishedAt   *string            `json:"published_at"`
	Envelope      outbox.Envelope    `json:"envelope"`
	Deliveries    []deliveryResponse `json:"deliveries,omitempty"`
}

func newEventResponse(e *outbox.Event, deliveries []outbox.Delivery) eventResponse {
	var publishedAt *string
	if e.PublishedAt != nil {
		s := e.PublishedAt.UTC().Format(timeFormat)
		publishedAt = &s
	}

	out := make([]deliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, newDeliveryResponse(d))
	}

	return eventResponse{
		EventID:       e.ID,
		EventType:     e.EventType,
		SchemaVersion: e.SchemaVersion,
		Status:        string(e.Status),
		Attempts:      e.Attempts,
		LastError:     e.LastError,
		CreatedAt:     e.CreatedAt.UTC().Format(timeFormat),
		PublishedAt:   publishedAt,
		Envelope:      e.Envelope(),
		Deliveries:    out,
	}
}

// handleGetEvent returns an outbox event with its full attempt history, which
// is how an operator finds out why something is stuck.
func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	event, err := s.events.GetEvent(r.Context(), id)
	if err != nil {
		writeEventError(w, err)
		return
	}

	deliveries, err := s.events.Deliveries(r.Context(), id)
	if err != nil {
		problem.Internal(w, err)
		return
	}

	writeJSON(w, http.StatusOK, newEventResponse(event, deliveries))
}

// handleReplayEvent re-publishes an event on demand.
//
// Replaying an already-published event is allowed and is the normal case: that
// is what the endpoint is for. The event id does not change, so a consumer that
// already handled it will deduplicate.
//
// The response is 202: publishing is at-least-once, so a successful attempt
// means the broker accepted the envelope, not that every consumer has processed
// it. Reporting 200 would overstate what is known.
func (s *Server) handleReplayEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	event, delivery, err := s.events.ReplayEvent(r.Context(), id, s.publisher)
	if err != nil {
		writeEventError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, struct {
		Event    eventResponse    `json:"event"`
		Delivery deliveryResponse `json:"delivery"`
	}{
		Event:    newEventResponse(event, nil),
		Delivery: newDeliveryResponse(*delivery),
	})
}

// writeEventError maps outbox storage errors onto the shared problem+json shape.
func writeEventError(w http.ResponseWriter, err error) {
	if errors.Is(err, postgres.ErrEventNotFound) {
		problem.Write(w, http.StatusNotFound, "event_not_found", "Event Not Found", err.Error())
		return
	}
	problem.Internal(w, err)
}
