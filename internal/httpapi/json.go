package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
)

// timeFormat is RFC 3339 with millisecond precision, so timestamps compare and
// sort as strings and do not vary in width between responses.
const timeFormat = "2006-01-02T15:04:05.000Z"

// maxRequestBody caps request bodies. Every endpoint in v1 takes a small JSON
// object; an unbounded reader is a denial-of-service invitation.
const maxRequestBody = 1 << 20 // 1 MiB

// readJSONBody reads the whole request body into memory, bounded, writing a
// problem+json response and returning false if it cannot.
//
// The bytes are kept rather than streamed because the idempotency fingerprint
// has to hash exactly what the client sent, and a decoder consumes the reader.
func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			problem.Write(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"Request Too Large", "request body exceeds 1 MiB")
			return nil, false
		}
		problem.Write(w, http.StatusBadRequest, "invalid_request", "Invalid Request",
			"could not read request body")
		return nil, false
	}
	return body, true
}

// unmarshalJSON parses body into dst. Unknown fields are rejected: a client that
// misspells "amount" should be told, not silently credited zero.
func unmarshalJSON(w http.ResponseWriter, body []byte, dst any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		problem.Write(w, http.StatusBadRequest, "invalid_json", "Invalid JSON", err.Error())
		return false
	}

	// A second value in the body means the client sent something we would
	// silently ignore. Reject it rather than guess which one was meant.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		problem.Write(w, http.StatusBadRequest, "invalid_json", "Invalid JSON",
			"body must contain exactly one JSON object")
		return false
	}
	return true
}

// decodeJSON reads and parses a request body for handlers that do not need the
// raw bytes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := readJSONBody(w, r)
	if !ok {
		return false
	}
	return unmarshalJSON(w, body, dst)
}

// parseUUIDField parses a UUID carried in a request body field.
func parseUUIDField(w http.ResponseWriter, field, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		problem.Write(w, http.StatusBadRequest, "invalid_uuid", "Invalid UUID",
			field+" is not a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// formatTimePtr renders an optional timestamp in the shared format.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(timeFormat)
	return &s
}
