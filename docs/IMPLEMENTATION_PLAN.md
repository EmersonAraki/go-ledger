# sumzero — Implementation Plan

A double-entry financial ledger exposed as a REST API in Go, with idempotent writes,
a transactional outbox for event replay, and reconciliation against external statements.

The name is the invariant: every transaction's entries must sum to zero.

---

## 1. Scope and non-goals

**In scope (v1)**

- Accounts and single-currency transfers between exactly two accounts.
- Strict idempotency on `POST /transactions` via an `Idempotency-Key` header.
- Double-entry ledger with a database-enforced zero-sum invariant.
- Transactional outbox with a versioned event envelope and a replay endpoint.
- CSV statement reconciliation with a persisted, queryable discrepancy report.

**Explicit non-goals (v1)**

- Multi-currency transactions (a transfer is single-currency; FX would need an FX
  account and two transactions — deferred).
- Multi-leg transactions with more than two entries. The schema supports N legs from
  day one; the API exposes only the two-leg case until there is a use case.
- Real Kafka publishing. The outbox relay ships with a logging publisher behind an
  interface; Kafka is a drop-in in phase 8.
- AuthN/AuthZ beyond a static API key middleware stub.

---

## 2. Stack decisions

| Concern | Choice | Why |
| --- | --- | --- |
| Router | `net/http` + `go-chi/chi/v5` | Stdlib-compatible handlers, middleware chain, URL params. No framework lock-in. |
| Postgres driver | `jackc/pgx/v5` + `pgxpool` | Native protocol, explicit `pgx.Tx`, per-transaction `BeginTx` options for isolation levels, and typed error inspection (`*pgconn.PgError`) for constraint violations — which the idempotency path depends on. |
| Migrations | Small embedded migrator (`internal/storage/postgres/migrate.go`) | ~200 lines over `embed.FS`: advisory-locked, one transaction per migration, checksums applied migrations so an edited file is refused rather than silently divergent. `golang-migrate` was the original pick, but it drags in a driver tree for databases this project will never use, and a project whose whole point is explicit SQL should not hide its migration path behind a dependency. |
| Money | `int64` minor units + ISO-4217 `currency` | Exact. No floats, no `decimal` dependency. `bigint` in Postgres. |
| IDs | UUIDv7 (`google/uuid` v1.6+) | Time-ordered, so B-tree inserts stay local and `ORDER BY id` approximates chronological order. |
| Logging | `log/slog` (stdlib) | Structured, no dependency. Request ID injected via context. |
| Tests | Real Postgres via `TEST_DATABASE_URL` | The correctness claims here are *database* claims (unique constraints, deferred triggers, row locks); mocking the DB would test nothing. Each test gets its own schema, so tests run in parallel and cannot see each other's rows. `testcontainers-go` was the original pick, but pointing at a connection string needs no Docker socket, works in CI via a `services:` container, and keeps the dependency out of `go.mod`. With the variable unset, integration tests skip rather than fail, so `go test ./...` works with nothing running. |
| Redis | Deferred to phase 8, cache-only | See §6.4 — Postgres is the sole source of truth for idempotency. |

---

## 3. Repository layout

```
cmd/
  api/main.go              # HTTP server, wiring, graceful shutdown
  migrate/main.go          # migrate up/down/version
  relay/main.go            # outbox relay worker (can also run in-process)
internal/
  config/                  # env-based config, validated at startup
  httpapi/
    router.go              # chi routes
    accounts.go            # account handlers
    transactions.go        # transaction handlers
    reconciliation.go
    events.go
    middleware/            # request ID, logging, recovery, idempotency
    problem/               # RFC 7807 error responses
  ledger/
    money.go               # Money{Amount int64, Currency string}
    account.go             # domain types + invariants
    transaction.go
    service.go             # CreateTransaction orchestration
  idempotency/
    store.go               # claim / complete / lookup
    fingerprint.go         # canonical request hashing
  outbox/
    event.go               # versioned envelope
    store.go
    relay.go               # FOR UPDATE SKIP LOCKED poller
    publisher.go           # Publisher interface + log/noop impls
  reconcile/
    csv.go                 # streaming statement parser
    matcher.go             # matching + discrepancy classification
    service.go
  storage/postgres/
    migrations/*.sql
    accounts.go transactions.go entries.go ...
  platform/
    pgtest/                # testcontainers helper shared by integration tests
docs/
  IMPLEMENTATION_PLAN.md   # this file
  openapi.yaml
  adr/                     # short decision records
```

Rule: `internal/ledger` holds domain logic and knows nothing about HTTP. `internal/httpapi`
translates HTTP to domain calls and domain errors to problem+json. `internal/storage/postgres`
is the only package that writes SQL.

---

## 4. Database schema

Written as migration `0001_init.sql`. Money is `bigint` minor units throughout.

> The DDL below was executed against PostgreSQL 16 while writing this plan, and the invariants it claims were exercised directly: a balanced two-leg transaction commits, an unbalanced one is rejected at `COMMIT` by the deferred trigger, a single-leg transaction is rejected, and an overdraft trips `accounts_balance_floor` while a system account with `allow_negative_balance` is allowed through. The concurrency claims in §6.2 and §7.1 were verified the same way, with two live sessions.

```sql
CREATE TYPE account_kind AS ENUM ('asset','liability','equity','revenue','expense');
CREATE TYPE entry_direction AS ENUM ('debit','credit');
CREATE TYPE outbox_status AS ENUM ('pending','published','failed');

CREATE TABLE accounts (
    id                     uuid PRIMARY KEY,
    name                   text        NOT NULL,
    kind                   account_kind NOT NULL,
    currency               char(3)     NOT NULL,
    -- Materialized balance, maintained inside the same tx as the entries.
    -- SUM(ledger_entries.signed_amount) remains the source of truth; this is a
    -- cache whose agreement is asserted by tests and by the reconciliation job.
    balance                bigint      NOT NULL DEFAULT 0,
    -- System accounts (external funding, fees) may go negative; user accounts may not.
    allow_negative_balance boolean     NOT NULL DEFAULT false,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_balance_floor
        CHECK (allow_negative_balance OR balance >= 0)
);

CREATE TABLE transactions (
    id           uuid PRIMARY KEY,
    external_ref text,                          -- statement/counterparty reference
    description  text        NOT NULL DEFAULT '',
    currency     char(3)     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX transactions_external_ref_key
    ON transactions (external_ref) WHERE external_ref IS NOT NULL;

CREATE TABLE ledger_entries (
    id             uuid PRIMARY KEY,
    transaction_id uuid            NOT NULL REFERENCES transactions(id) ON DELETE RESTRICT,
    account_id     uuid            NOT NULL REFERENCES accounts(id)     ON DELETE RESTRICT,
    direction      entry_direction NOT NULL,
    amount         bigint          NOT NULL CHECK (amount > 0),
    currency       char(3)         NOT NULL,
    -- Debits positive, credits negative. The zero-sum check reads this column.
    signed_amount  bigint GENERATED ALWAYS AS
        (CASE WHEN direction = 'debit' THEN amount ELSE -amount END) STORED,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_transaction_id_idx ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_account_id_created_at_idx
    ON ledger_entries (account_id, created_at DESC);
```

**Zero-sum enforcement.** A row-level `CHECK` cannot see sibling rows, so the invariant is a
`DEFERRABLE INITIALLY DEFERRED` constraint trigger. Deferred means it fires at `COMMIT`, after
all of the transaction's entries exist — the balanced state is checked, not each intermediate
insert.

```sql
CREATE FUNCTION assert_transaction_balanced() RETURNS trigger AS $$
DECLARE
    txid      uuid;
    imbalance bigint;
    leg_count int;
BEGIN
    -- NEW is unset on DELETE, so branch on TG_OP rather than COALESCE-ing across them.
    IF TG_OP = 'DELETE' THEN
        txid := OLD.transaction_id;
    ELSE
        txid := NEW.transaction_id;
    END IF;

    SELECT COALESCE(SUM(signed_amount), 0), COUNT(*)
      INTO imbalance, leg_count
      FROM ledger_entries
     WHERE transaction_id = txid;

    -- All legs deleted: nothing left to balance.
    IF leg_count = 0 THEN
        RETURN NULL;
    END IF;
    IF leg_count < 2 THEN
        RAISE EXCEPTION 'transaction % has % leg(s), at least 2 required', txid, leg_count
            USING ERRCODE = 'check_violation';
    END IF;
    IF imbalance <> 0 THEN
        RAISE EXCEPTION 'transaction % is unbalanced by %', txid, imbalance
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();
```

Additionally, `ledger_entries` is append-only in practice: a `REVOKE UPDATE, DELETE` grant on the
application role makes corrections happen through reversing transactions, not edits. Reversal is a
new transaction with the legs swapped and `reverses_transaction_id` set (migration `0006`).

**Idempotency keys.**

```sql
CREATE TABLE idempotency_keys (
    key             text        NOT NULL,
    endpoint        text        NOT NULL,   -- 'POST /transactions'
    request_hash    bytea       NOT NULL,   -- SHA-256 of the canonical request
    response_status int,
    -- json, not jsonb: jsonb sorts keys and reformats whitespace, so a replayed
    -- response would not be byte-identical to the original (migration 0002).
    response_body   json,
    transaction_id  uuid REFERENCES transactions(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (key, endpoint)
);
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
```

The composite primary key *is* the concurrency control. There is no `state` column and no
in-flight bookkeeping — see §6.2 for why.

**Outbox.**

```sql
CREATE TABLE outbox_events (
    id             uuid PRIMARY KEY,          -- also the event_id in the envelope
    aggregate_type text          NOT NULL,    -- 'transaction'
    aggregate_id   uuid          NOT NULL,
    event_type     text          NOT NULL,    -- 'ledger.transaction.created'
    schema_version int           NOT NULL,    -- 1
    payload        jsonb         NOT NULL,
    status         outbox_status NOT NULL DEFAULT 'pending',
    attempts       int           NOT NULL DEFAULT 0,
    last_error     text,
    available_at   timestamptz   NOT NULL DEFAULT now(),  -- backoff gate
    published_at   timestamptz,
    created_at     timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (available_at) WHERE status <> 'published';

CREATE TABLE outbox_deliveries (
    id            bigserial PRIMARY KEY,
    event_id      uuid NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    attempted_at  timestamptz NOT NULL DEFAULT now(),
    succeeded     boolean NOT NULL,
    trigger       text NOT NULL,              -- 'relay' | 'manual_replay'
    error         text
);
```

**Reconciliation.**

```sql
CREATE TABLE reconciliation_runs (
    id                uuid PRIMARY KEY,
    source_name       text        NOT NULL,
    statement_rows    int         NOT NULL,
    matched_count     int         NOT NULL,
    discrepancy_count int         NOT NULL,
    window_start      timestamptz,
    window_end        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reconciliation_discrepancies (
    id             bigserial PRIMARY KEY,
    run_id         uuid  NOT NULL REFERENCES reconciliation_runs(id) ON DELETE CASCADE,
    kind           text  NOT NULL,   -- see §8
    statement_ref  text,
    transaction_id uuid,
    details        jsonb NOT NULL    -- expected vs actual
);
```

---

## 5. API surface

All errors are `application/problem+json` (RFC 7807) with a stable machine-readable `type`.

| Method | Path | Notes |
| --- | --- | --- |
| `POST` | `/accounts` | `{name, kind, currency}` → `201` with the account. |
| `GET` | `/accounts/{id}` | Balance + metadata. `404` if unknown. |
| `GET` | `/accounts/{id}/entries` | Keyset-paginated statement (`?after=&limit=`). |
| `POST` | `/transactions` | **Requires `Idempotency-Key`.** `{debit_account_id, credit_account_id, amount, currency, description, external_ref}` → `201`. |
| `GET` | `/transactions/{id}` | Transaction with its legs. |
| `POST` | `/events/{id}/replay` | Re-publishes an outbox event. `202`. |
| `GET` | `/events/{id}` | Event envelope + delivery history. |
| `POST` | `/reconciliation` | `multipart/form-data` CSV upload → `201` with run summary. |
| `GET` | `/reconciliation/{id}` | Run result + paginated discrepancies. |
| `GET` | `/healthz`, `/readyz` | Liveness / DB readiness. |

Amounts cross the wire as integer minor units plus an explicit currency
(`{"amount": 12345, "currency": "BRL"}` = R$123.45). Never a decimal string, never a float.

---

## 6. Idempotency design

### 6.1 Request fingerprint

`request_hash = SHA256(method || "\n" || path || "\n" || canonicalJSON(body))`, where
`canonicalJSON` unmarshals into `map[string]any` and re-marshals with sorted keys, so
whitespace and field ordering do not change the hash. The hash exists to catch a *client bug*:
the same key reused for a different payload.

### 6.2 The claim protocol

The whole thing runs in **one** database transaction. That is the key decision.

```
BEGIN;
  INSERT INTO idempotency_keys (key, endpoint, request_hash)
  VALUES ($1, $2, $3)
  ON CONFLICT (key, endpoint) DO NOTHING;
  -- rows affected == 1  -> we own this key
  -- rows affected == 0  -> someone else owns it

  [if we own it]
    ... lock accounts, insert transaction + entries, update balances, insert outbox event ...
    UPDATE idempotency_keys
       SET response_status = 201, response_body = $4, transaction_id = $5
     WHERE key = $1 AND endpoint = $2;
COMMIT;
```

Because the claim row and the ledger writes commit together, there is no window in which a key is
claimed but the work is missing. **There are no stale in-flight rows to garbage-collect**, because a
crashed request rolls the claim back along with everything else.

The concurrency behaviour falls out of Postgres semantics: when request B hits
`ON CONFLICT DO NOTHING` against a row inserted by an *uncommitted* request A, B **blocks on A's
index tuple lock** until A commits or aborts. Then:

- A committed → B's insert affects 0 rows, B reads the completed row and replays the stored response.
- A aborted → B's insert succeeds and B becomes the owner.

So the duplicate never sees a half-finished state and never has to poll. Both branches were
verified against PostgreSQL 16 with two concurrent sessions: when A committed, B blocked for the
remainder of A's transaction, its insert reported 0 rows, and it then read A's stored `201`
response; when A rolled back, B blocked, its insert reported 1 row, and it became the owner with
no leftover row from A. The cost is that B holds a
pooled connection for the duration of A's transaction. That is acceptable while the write path stays
short (single-digit milliseconds); the escalation path, if it ever isn't, is a two-phase claim in a
separate transaction returning `409 request_in_progress` — which then does require a janitor for
abandoned claims. Documented in `docs/adr/0002-idempotency.md`, not built.

### 6.3 Response matrix

| Situation | Status | Body / headers |
| --- | --- | --- |
| Header missing or empty | `400` | `type: idempotency_key_required` |
| Key length > 255 or non-printable | `400` | `type: idempotency_key_invalid` |
| First time | `201` | The created transaction |
| Replay, same fingerprint | stored status | Byte-identical stored body + `Idempotency-Replayed: true` |
| Replay, different fingerprint | `422` | `type: idempotency_key_reuse` |
| Underlying request invalid | `400`/`422` | Failures are **not** stored; the key stays claimable |

That last row matters: only successful responses are cached. A client that fixes a validation error
and retries with the same key must be able to succeed.

### 6.4 Redis: cache only, phase 8, gated on evidence

Postgres remains the sole authority for *ownership*. Redis, if added, is a read-through cache of
**completed** responses only, written after commit, TTL 24h, on cache miss falling through to
Postgres. Nothing about correctness depends on it — a cold, flushed, or unreachable Redis degrades
latency and nothing else. It is deliberately never consulted to decide who owns a key, because
`SETNX` and the DB transaction cannot commit atomically and any scheme that pretends otherwise is
either lossy or a distributed lock with a lease. Ship it only if benchmarks in phase 5 show the
Postgres lookup is actually a bottleneck.

---

## 7. Concurrency and isolation

### 7.1 Which isolation level, and why

Two invariants are at risk under concurrency:

1. **Zero-sum per transaction** — never at risk. All of a transaction's entries are inserted by one
   session in one transaction, and the deferred constraint trigger evaluates them at commit.
   Isolation level is irrelevant here.
2. **Balance floor (no unauthorized negative balance)** — the real hazard, and a classic
   **write skew**: two concurrent withdrawals from a balance of 100 each read 100, each check
   `100 - 60 >= 0`, and both commit, leaving -20.

This was measured rather than assumed. Two concurrent withdrawals of 60 from a balance of 100,
each enforcing the balance check itself, run against PostgreSQL 16:

| Strategy | Commits | Final balance | Loser sees |
| --- | --- | --- | --- |
| `REPEATABLE READ`, no locking | 2 | **-20 (overdrawn)** | — |
| `SERIALIZABLE`, no locking | 1 | 40 | `40001 could not serialize access due to read/write dependencies` |
| `READ COMMITTED` + `SELECT … FOR UPDATE` | 1 | 40 | `insufficient_funds: saw balance 40` |

The instinct is to reach for `REPEATABLE READ`, but in Postgres that is *snapshot isolation*, and
snapshot isolation does **not** prevent write skew: the two withdrawals insert different entry rows,
so there is no update conflict to detect and both commit. It does not fix the bug.

One nuance worth recording, because it is easy to draw the wrong general lesson: `REPEATABLE READ`
*does* catch this when both transactions `UPDATE` a shared row — with a materialized `balance`
column, the second gets `40001 could not serialize access due to concurrent update` (also measured).
So `REPEATABLE READ`'s protection here is an artifact of writing a common row, not a guarantee about
the invariant. Any check computed from `SUM(ledger_entries)` — which reconciliation and any future
multi-account rule will do — is unprotected. The design should not depend on that accident.

That leaves two correct options:

- **`SERIALIZABLE`** (Postgres SSI): correct, but every caller needs a `40001` retry loop, the loser
  discovers the conflict only at commit after doing all its work, and throughput degrades on hot
  accounts.
- **`READ COMMITTED` + explicit row locks**: contenders serialize on the row lock instead of
  aborting. The loser's `SELECT` then reads the *fresh* committed balance and fails its own check,
  producing a clean domain error (`insufficient_funds`) instead of a retryable database abort.

**Decision: `READ COMMITTED` with explicit pessimistic locking on the write path.** It is the only
one of the three that both preserves the invariant and returns the client a meaningful error the
first time, with no retry loop in the service.

```sql
SELECT id, balance, currency, allow_negative_balance
  FROM accounts
 WHERE id = ANY($1)
 ORDER BY id          -- deterministic order: prevents A->B / B->A deadlocks
   FOR UPDATE;
```

Locking in sorted `id` order is what makes concurrent `A->B` and `B->A` transfers deadlock-free.
The `FOR UPDATE` read gives an up-to-date balance, the floor is checked in Go, and the
`accounts_balance_floor` CHECK constraint is the backstop if the application logic is ever wrong.

Read paths get different treatment: reconciliation opens a **`REPEATABLE READ` read-only**
transaction, because it needs one consistent point-in-time snapshot across many queries. That is the
case snapshot isolation is genuinely right for.

`docs/adr/0001-isolation.md` records this with the table above plus phase-5 throughput numbers.

### 7.2 The concurrency test that has to pass

`TestConcurrentSameIdempotencyKey`: 50 goroutines fire the identical `POST /transactions` with one
shared key against a real Postgres. Assertions:

- exactly one response without `Idempotency-Replayed`, 49 with it;
- all 50 bodies byte-identical;
- `SELECT COUNT(*) FROM ledger_entries` = 2;
- `SELECT COUNT(*) FROM outbox_events` = 1;
- debited account balance moved by exactly one transfer amount.

`TestConcurrentTransfersRespectBalanceFloor`: 20 goroutines each withdraw 60 from a balance of 1000
with *distinct* keys — expect exactly 16 successes, 4 `422 insufficient_funds`, final balance 40,
and `SUM(signed_amount) = 0` across the whole ledger. This is the test that fails under
`REPEATABLE READ` and passes under the chosen design.

Everything runs under `go test -race`.

---

## 8. Outbox and replay

**Envelope, versioned from the first commit.** The payload is nested under a stable envelope so
consumers can route on `event_type` + `schema_version` without parsing the body.

```json
{
  "event_id": "0192f3a1-...",
  "event_type": "ledger.transaction.created",
  "schema_version": 1,
  "occurred_at": "2026-09-02T12:00:00Z",
  "producer": "sumzero",
  "aggregate": { "type": "transaction", "id": "0192f3a0-..." },
  "payload": {
    "transaction_id": "0192f3a0-...",
    "currency": "BRL",
    "external_ref": "TRX-88231",
    "entries": [
      { "account_id": "...", "direction": "debit",  "amount": 12345 },
      { "account_id": "...", "direction": "credit", "amount": 12345 }
    ]
  }
}
```

Compatibility rule, enforced by a golden-file test in `internal/outbox`: within a `schema_version`,
fields may only be *added*. Removing or retyping a field bumps the version, and the relay may then
emit both versions during a migration window.

**Relay.** A goroutine (and a standalone `cmd/relay` for scaling out) polling every 500ms:

```sql
SELECT * FROM outbox_events
 WHERE status <> 'published' AND available_at <= now()
 ORDER BY created_at
 FOR UPDATE SKIP LOCKED
 LIMIT 100;
```

`SKIP LOCKED` lets multiple relay instances run without coordination. On failure: `attempts++`,
`last_error`, and `available_at = now() + backoff(attempts)` (exponential, capped at 5 min); after
N attempts, `status = 'failed'` and it stops being retried automatically — which is exactly what
the replay endpoint is for.

Delivery is **at-least-once**. `event_id` is stable across every delivery and replay, so consumers
dedupe on it. Ordering is only guaranteed per aggregate, by using `aggregate_id` as the partition
key when Kafka lands.

**`POST /events/{id}/replay`** re-publishes the event with the *same* `event_id`, appends a row to
`outbox_deliveries` with `trigger = 'manual_replay'`, and resets `status`/`available_at` so a failed
event re-enters the relay. It returns `202` with the delivery record. Replaying an
already-published event is allowed and is the normal case — that is the point of the endpoint.
`404` for an unknown id.

---

## 9. Reconciliation

**Input.** `multipart/form-data`, field `file`, a CSV with a header row:

```
external_ref,posted_at,debit_account_id,credit_account_id,amount,currency
TRX-88231,2026-09-01T10:00:00Z,0192...,0192...,12345,BRL
```

Parsed with `encoding/csv` as a stream (`Read()` per row, never `ReadAll`) so memory stays flat;
`http.MaxBytesReader` caps the upload; malformed rows are collected as `unparseable_row`
discrepancies rather than aborting the run.

**Matching.** Two passes over a `REPEATABLE READ` read-only snapshot:

1. Exact match on `external_ref`. Compare amount, currency, both account ids, and posting date.
2. Anything unmatched falls to a heuristic pass keyed on
   `(debit_account_id, credit_account_id, amount, currency)` within a ±1 day window, flagged as a
   `probable_match` rather than silently reconciled.

Ledger transactions inside the statement's time window that no statement row claimed are reported
as `missing_in_statement`.

**Discrepancy kinds.** `missing_in_ledger`, `missing_in_statement`, `amount_mismatch`,
`currency_mismatch`, `account_mismatch`, `date_mismatch`, `duplicate_in_statement`,
`unparseable_row`. Each carries `details: {expected, actual}`.

The run is persisted, so `GET /reconciliation/{id}` returns it without re-uploading, and runs are
comparable over time. Reconciliation is strictly read-only against the ledger: it *reports*, it
never auto-corrects. Corrections are reversing transactions posted through the normal API.

**Balance drift is a separate job, not part of a run.** Asserting
`accounts.balance == SUM(ledger_entries.signed_amount)` is what keeps the materialized balance
honest, but it cannot be part of a statement run. A derived balance is the sum of an account's
entire history, so no predicate bounds it — an attempt to scope the check to the statement's own
window was measured *slower* than the unscoped query it replaced (~120 ms against ~110 ms over
400k entries, three runs each), because scoping chooses which accounts to check while each one's full history is summed
regardless. On an unauthenticated endpoint that made a one-row CSV naming any active day worth a
full ledger scan.

So it runs as `cmd/driftsweep`: a scheduled job that walks every account in keyset-paginated pages,
comparing each cached balance against its entries within a single statement — hence a single
snapshot, so a transfer committing mid-sweep is never misreported as drift. It records a
`reconciliation_runs` row under the source name `balance-drift-sweep` only when it finds something,
so the report is readable through the same `GET /reconciliation/{id}`, and the absence of a run is
itself the signal that the ledger is sound. Exit status 2 means drift, so cron alerts without
parsing output.

The rule this settles: **every query on the upload path needs a bound expressible in terms of the
upload.** `MaxWindowDays`, `MaxLedgerWindowRows` and `MaxUnreconcilableRows` are those bounds.
A check that cannot have one does not belong on the path.

---

## 10. Delivery phases

Each phase ends on a green `make check` (lint + `go test -race ./...`) and its own commit.

| # | Phase | Deliverable | Done when |
| --- | --- | --- | --- |
| 0 | Scaffolding | **Done.** Module, layout, `docker-compose.yml` (postgres, redis), `Makefile`, golangci-lint, GitHub Actions CI, `/healthz` + `/readyz`, graceful shutdown | `make check` green; server boots, serves both probes, and exits cleanly on SIGTERM |
| 1 | Schema | **Done.** Migration `0001_init`, `cmd/migrate` (up/down/version), `pgtest` schema-per-test helper | `migrate up` is idempotent and `down`/`up` round-trips; 8 tests cover the deferred trigger, balance floor, and partial unique index |
| 2 | Accounts + transactions | **Done.** `POST/GET /accounts`, `POST/GET /transactions` (no idempotency yet), problem+json errors, `internal/ledger` domain layer | Integration tests cover happy path, unknown account, currency mismatch, non-positive and self transfer, insufficient funds, duplicate `external_ref`, malformed body/UUID; every test asserts the ledger still sums to zero |
| 3 | Idempotency | **Done.** `internal/idempotency` (key validation, canonical fingerprint), single-transaction claim in the store, full response matrix (§6.3) | Every row of the §6.3 matrix has a test, plus the §7.2 concurrency proof: 30 racing identical requests, exactly one execution |
| 4 | Double-entry hardening | Reversal endpoint, account statement endpoint. Balance drift **shipped in phase 7** as `cmd/driftsweep`. **Ordered `FOR UPDATE` locking, the balance floor and the materialized balance moved into phase 2** — shipping a money-transfer endpoint with a known write-skew race, when the fix is a few lines already designed in §7.1, was not defensible | `SUM(signed_amount) = 0` holds after every test in the suite |
| 5 | Concurrency proof | The two tests in §7.2 + a `SERIALIZABLE` vs `READ COMMITTED + FOR UPDATE` benchmark | Both tests pass under `-race`; ADR 0001 written with the numbers |
| 6 | Outbox + replay | **Done.** Versioned envelope, event written in the transfer's transaction, relay with `SKIP LOCKED` + exponential backoff, `GET /events/{id}`, `POST /events/{id}/replay`, in-process relay plus standalone `cmd/relay` | Golden-file envelope test; four concurrent relays proved not to double publish; a rejected transfer proved to leave no event; replay produces a second delivery row under one `event_id` |
| 7 | Reconciliation | **Done.** Streaming CSV parser, two-pass matcher, persisted runs, `POST /reconciliation` and `GET /reconciliation/{id}` with keyset pagination, and `cmd/driftsweep` for the balance-drift check that could not be bounded on the request path (§9) | A fixture statement with one of each discrepancy kind is classified correctly; a separate test proves reconciliation never writes to the ledger; every processing limit has a test at its boundary, and each was verified by reverting the bound and watching the test go red |
| 8 | Optional extras | Redis response cache (only if phase 5 justifies it), Kafka/Redpanda publisher, OpenAPI spec, Prometheus metrics | Publisher swap requires no changes outside `outbox.Publisher` wiring |

Phases 0–7 are the project. Phase 8 is opportunistic.

---

## 11. Operational details settled up front

- **Config** from environment, parsed and validated at startup, failing fast:
  `DATABASE_URL`, `HTTP_ADDR`, `LOG_LEVEL`, `IDEMPOTENCY_TTL`, `OUTBOX_POLL_INTERVAL`,
  `OUTBOX_MAX_ATTEMPTS`, `SHUTDOWN_TIMEOUT`.
- **Errors**: sentinel domain errors (`ledger.ErrAccountNotFound`, `ErrInsufficientFunds`,
  `ErrCurrencyMismatch`) mapped centrally to problem+json in one place; handlers never build error
  bodies inline.
- **Every request** gets a request ID (honouring an inbound `X-Request-Id`), logged with method,
  path, status, duration, and — for `POST /transactions` — the idempotency key.
- **Timeouts** everywhere: `http.Server` read/write/idle, a per-request context deadline, and
  `pgxpool` acquire timeouts. No unbounded waits.
- **Cleanup job**: `DELETE FROM idempotency_keys WHERE expires_at < now()` and archival of
  `published` outbox events older than 30 days, both in a single ticker goroutine, batched.
- **Migrations are forward-only in CI**; `down` migrations exist for local development only.

## 12. Decisions recorded as ADRs

1. `0001-isolation.md` — `READ COMMITTED` + ordered `FOR UPDATE` over `SERIALIZABLE`; why
   `REPEATABLE READ` is not a candidate (§7.1).
2. `0002-idempotency.md` — single-transaction claim over two-phase claim; the blocking trade-off.
3. `0003-money.md` — `int64` minor units over `NUMERIC`/decimal library.
4. `0004-event-envelope.md` — versioned envelope, additive-only within a version.
5. `0005-balance-materialization.md` — cached `balance` column with reconciliation-backed drift
   detection, over pure `SUM()` on read.
