# go-ledger

**sumzero** — a Go REST API implementing a double-entry financial ledger with
idempotent writes, transactional-outbox event replay, and reconciliation against
external statements.

The name is the invariant: every transaction's entries must sum to zero, and the
database enforces it.

## Status

Phases 0-2 of [the implementation plan](docs/IMPLEMENTATION_PLAN.md) are
complete: scaffolding, the schema with its invariants under test, and the
accounts and transactions endpoints. Idempotency (phase 3) is next -- until then
`POST /transactions` is not safe to retry, since a repeated request posts a
second transfer.

| Method | Path | |
| --- | --- | --- |
| `POST` | `/accounts` | Create an account (always starts at zero) |
| `GET` | `/accounts/{id}` | Balance and details |
| `POST` | `/transactions` | Transfer between two accounts |
| `GET` | `/transactions/{id}` | Transaction with both legs |
| `GET` | `/healthz`, `/readyz` | Liveness, readiness |

Money crosses the wire as integer minor units plus a currency
(`{"amount": 12345, "currency": "BRL"}` is R$123.45) -- never a float.
In `POST /transactions` the money ends up in `debit_account_id`, funded by
`credit_account_id`; the names follow accounting, not banking.

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
