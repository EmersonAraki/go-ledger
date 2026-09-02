// Package problem renders RFC 7807 "application/problem+json" error responses.
//
// Every error the API returns goes through here, so clients get one shape and a
// stable machine-readable Type they can branch on.
package problem

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ContentType is the media type required by RFC 7807.
const ContentType = "application/problem+json"

// Problem is the response body. Detail is human-facing; Type is what clients
// should actually branch on.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Write renders p to w. It is the only place in the codebase that writes an
// error body, which keeps the wire format from drifting handler to handler.
func Write(w http.ResponseWriter, status int, typ, title, detail string) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	body := Problem{Type: typ, Title: title, Status: status, Detail: detail}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write problem response", "error", err)
	}
}

// Internal reports an unexpected failure. The underlying error is logged but
// never sent to the client.
func Internal(w http.ResponseWriter, err error) {
	slog.Error("internal error", "error", err)
	Write(w, http.StatusInternalServerError, "internal_error", "Internal Server Error", "")
}
