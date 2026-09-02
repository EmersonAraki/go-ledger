// Package idempotency computes the request fingerprint and validates the
// Idempotency-Key header. It is deliberately transport-agnostic: the storage
// layer owns the claim protocol, this package only decides what a request's
// identity is.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// HeaderKey is the request header carrying the client's key.
const HeaderKey = "Idempotency-Key"

// HeaderReplayed marks a response served from a previous request's stored
// result, so a client can distinguish a replay from a fresh execution.
const HeaderReplayed = "Idempotency-Replayed"

// MaxKeyLength bounds the Idempotency-Key header. The column is unbounded text,
// but accepting arbitrarily long keys lets a client bloat the table.
const MaxKeyLength = 255

// Validation failures, mapped to 400 by the HTTP layer.
var (
	// ErrKeyRequired means the Idempotency-Key header was absent or empty.
	ErrKeyRequired = errors.New("Idempotency-Key header is required")

	// ErrKeyInvalid means the key is too long or contains non-printable bytes.
	ErrKeyInvalid = errors.New("Idempotency-Key is malformed")
)

// ValidateKey checks the header value. Keys must be non-empty printable ASCII
// so that they are safe to log and to echo back in error messages.
func ValidateKey(key string) error {
	if key == "" {
		return ErrKeyRequired
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("%w: longer than %d bytes", ErrKeyInvalid, MaxKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7E {
			return fmt.Errorf("%w: contains a non-printable byte", ErrKeyInvalid)
		}
	}
	return nil
}

// Fingerprint identifies the request a key was used for, as
// SHA-256(method \n path \n canonical(body)).
//
// The body is canonicalised by round-tripping through Go's JSON encoder, which
// sorts object keys, so whitespace and field ordering do not change the
// fingerprint. Two requests that mean the same thing therefore hash the same.
//
// This exists to catch a client bug -- the same key reused for a genuinely
// different payload -- not to authenticate anything.
func Fingerprint(method, path string, body []byte) ([]byte, error) {
	canonical, err := canonicalJSON(body)
	if err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{'\n'})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	return h.Sum(nil), nil
}

// canonicalJSON re-encodes body with object keys sorted. An empty body is
// canonical as-is, so a key can still be used on a request without one.
func canonicalJSON(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}

	// UseNumber keeps numbers as their original literal text. Decoding into a
	// plain `any` would turn every number into a float64, which silently rounds
	// integers above 2^53 -- and amounts here are int64 minor units, so two
	// genuinely different large transfers could fingerprint identically.
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonicalise request body: %w", err)
	}

	// encoding/json sorts map[string]any keys on marshal, which is what makes
	// this canonical.
	canonical, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicalise request body: %w", err)
	}
	return canonical, nil
}
