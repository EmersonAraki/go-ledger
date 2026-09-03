package ledger

import "errors"

// Idempotency errors.
var (
	// ErrIdempotencyKeyReuse means the key was already used for a request with
	// a different payload. Almost always a client bug -- a key generated once
	// and then reused for a second, different transfer.
	ErrIdempotencyKeyReuse = errors.New("idempotency key already used for a different request")
)

// Claim identifies a request for idempotency purposes.
type Claim struct {
	// Key is the client-supplied Idempotency-Key header.
	Key string
	// Endpoint scopes the key, so the same key on two different endpoints does
	// not collide.
	Endpoint string
	// Fingerprint is the hash of the request this key was first used for.
	Fingerprint []byte
}

// RenderFunc turns a committed transaction into the response the client will
// receive. It runs inside the database transaction that created the
// transaction, because the bytes it returns are stored alongside the claim and
// must commit atomically with it.
//
// Passing rendering in as a callback is what lets the storage layer own the
// transaction boundary while the HTTP layer stays the only thing that knows
// what a response body looks like.
type RenderFunc func(*Transaction) (status int, body []byte, err error)

// Result is the outcome of an idempotent transfer.
type Result struct {
	// Status and Body are what to send the client: freshly rendered on the
	// first request, replayed verbatim from storage on a duplicate.
	Status int
	Body   []byte
	// Replayed reports whether this response came from storage rather than from
	// work done now.
	Replayed bool
	// Transaction is set only on a first request. A replay deliberately does
	// not re-read it: the stored body is the response.
	Transaction *Transaction
}
