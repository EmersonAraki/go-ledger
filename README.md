# go-ledger

**sumzero** — a Go REST API implementing a double-entry financial ledger with
idempotent writes, transactional-outbox event replay, and reconciliation against
external statements.

The name is the invariant: every transaction's entries must sum to zero, and the
database enforces it.

## Status

Phases 0-2 of [the implementation plan](docs/IMPLEMENTATION_PLAN.md) are
complete, plus phases 6 and 7: scaffolding, the schema with its invariants under
test, the accounts and transactions endpoints, idempotent transfers, the
transactional outbox with event replay, and statement reconciliation. What
remains are the smaller phase 4/5 leftovers (reversal and account statement
endpoints) and the optional phase 8 extras.

| Method | Path | |
| --- | --- | --- |
| `POST` | `/accounts` | Create an account (always starts at zero) |
| `GET` | `/accounts/{id}` | Balance and details |
| `POST` | `/transactions` | Transfer between two accounts (requires `Idempotency-Key`) |
| `GET` | `/transactions/{id}` | Transaction with both legs |
| `GET` | `/events/{id}` | Outbox event, its envelope and delivery history |
| `POST` | `/events/{id}/replay` | Re-publish an event |
| `POST` | `/reconciliation` | Upload a CSV statement, get a discrepancy report |
| `GET` | `/reconciliation/{id}` | A stored run, findings keyset-paginated |
| `GET` | `/healthz`, `/readyz` | Liveness, readiness |

Money crosses the wire as integer minor units plus a currency
(`{"amount": 12345, "currency": "BRL"}` is R$123.45) -- never a float.
In `POST /transactions` the money ends up in `debit_account_id`, funded by
`credit_account_id`; the names follow accounting, not banking.

`POST /transactions` requires an `Idempotency-Key` header and is safe to retry.
A repeat of the same request returns the original response byte-for-byte with
`Idempotency-Replayed: true` and does no new work; the same key with a different
payload is refused with `422`. A request that fails does not consume its key, so
a client can fix the payload and retry with the same one.

## Quick start

```sh
make up        # start Postgres and Redis
make migrate   # apply the schema
make run       # serve the API on :8080
```

```sh
curl localhost:8080/healthz   # liveness  -> {"status":"ok"}
curl localhost:8080/readyz    # readiness -> 503 if Postgres is unreachable
```

## Development

```sh
make check     # gofmt + go vet + golangci-lint + go test -race   (what CI runs)
make test      # integration tests against TEST_DATABASE_URL
make help      # list all targets
```

Integration tests need a real PostgreSQL, because what they verify are database
behaviours: a deferred constraint trigger, a partial unique index, row locking.
Point `TEST_DATABASE_URL` at a scratch database and each test builds its own
isolated schema. With the variable unset those tests skip, so `go test ./...`
still runs with no services up.

## Layout

| Path | Contents |
| --- | --- |
| `cmd/api` | HTTP server: wiring, graceful shutdown |
| `cmd/migrate` | Schema migration CLI (`up`, `down [n]`, `version`) |
| `internal/httpapi` | Routing, handlers, problem+json errors |
| `internal/storage/postgres` | Migrations and all SQL |
| `internal/platform/pgtest` | Isolated-schema test harness |
| `docs/IMPLEMENTATION_PLAN.md` | Design decisions and delivery phases |

`internal/httpapi` translates HTTP to domain calls; `internal/storage/postgres`
is the only package that writes SQL. Domain logic (arriving in phase 2) knows
about neither.

## Design notes

The decisions worth knowing before reading the code, all detailed in the plan:

- **Money** is `int64` minor units plus an ISO-4217 currency. Never a float.
- **Zero-sum** is enforced by a `DEFERRABLE INITIALLY DEFERRED` constraint
  trigger, checked at `COMMIT` once every leg exists — not by application code.
- **Isolation** is `READ COMMITTED` with ordered `SELECT … FOR UPDATE`, not
  `REPEATABLE READ`: in Postgres that is snapshot isolation, which does not
  prevent the write skew that lets two concurrent withdrawals overdraw an
  account. Measured, not assumed — see plan §7.1.
- **Idempotency** claims a key with `ON CONFLICT DO NOTHING` in the *same*
  transaction as the ledger writes, so a crashed request leaves no half-claimed
  state and there are no stale rows to garbage-collect — see plan §6.2.
- **The outbox** event is written in that same transaction, so an event can never
  describe a transfer that did not happen and a transfer can never happen
  without its event. Delivery is at-least-once with a stable `event_id`, so
  consumers deduplicate on it — see plan §8.
- **Reconciliation** is strictly read-only against the ledger: it reports
  disagreements and never corrects them, because a correction is a reversing
  transaction with its own audit trail. It reads under `REPEATABLE READ` so every
  query sees one instant — the one place snapshot isolation is the right tool,
  in contrast to the write path above — see plan §9.
