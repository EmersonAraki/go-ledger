package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
)

// timeFormat is RFC 3339 with millisecond precision, so timestamps compare and
// sort as strings and do not vary in width between responses.
const timeFormat = "2006-01-02T15:04:05.000Z"

// maxRequestBody caps request bodies. Every endpoint in v1 takes a small JSON
// object; an unbounded reader is a denial-of-service invitation.
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSON reads a JSON request body into dst, writing a problem+json
// response and returning false if it cannot. Unknown fields are rejected: a
// client that misspells "amount" should be told, not silently credited zero.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			problem.Write(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"Request Too Large", "request body exceeds 1 MiB")
			return false
		}
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
