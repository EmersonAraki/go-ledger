-- Core ledger schema.
--
-- Money is always bigint minor units plus an explicit ISO-4217 currency.
-- No floats, no NUMERIC: exact integer arithmetic end to end.

CREATE TYPE account_kind AS ENUM ('asset','liability','equity','revenue','expense');
CREATE TYPE entry_direction AS ENUM ('debit','credit');
CREATE TYPE outbox_status AS ENUM ('pending','published','failed');

CREATE TABLE accounts (
    id                     uuid PRIMARY KEY,
    name                   text         NOT NULL,
    kind                   account_kind NOT NULL,
    currency               char(3)      NOT NULL,
    -- Materialized balance, maintained in the same transaction as the entries.
    -- SUM(ledger_entries.signed_amount) stays the source of truth; this is a
    -- cache, and reconciliation reports any drift between the two.
    balance                bigint       NOT NULL DEFAULT 0,
    -- System accounts (external funding, fees) may go negative; user accounts
    -- may not. This is the backstop behind the application's own check.
    allow_negative_balance boolean      NOT NULL DEFAULT false,
    created_at             timestamptz  NOT NULL DEFAULT now(),
    updated_at             timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT accounts_balance_floor
        CHECK (allow_negative_balance OR balance >= 0)
);

CREATE TABLE transactions (
    id           uuid PRIMARY KEY,
    external_ref text,
    description  text        NOT NULL DEFAULT '',
    currency     char(3)     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Partial unique index: many transactions may have no external reference,
-- but a given reference may only appear once.
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

-- The zero-sum invariant.
--
-- A row-level CHECK cannot see sibling rows, so this is a constraint trigger
-- declared DEFERRABLE INITIALLY DEFERRED: it fires at COMMIT, once every leg of
-- the transaction exists, rather than on each intermediate insert.
CREATE FUNCTION assert_transaction_balanced() RETURNS trigger AS $$
DECLARE
    txid      uuid;
    imbalance bigint;
    leg_count int;
BEGIN
    -- NEW is unset on DELETE, so branch on TG_OP rather than COALESCE across them.
    IF TG_OP = 'DELETE' THEN
        txid := OLD.transaction_id;
    ELSE
        txid := NEW.transaction_id;
    END IF;

    SELECT COALESCE(SUM(signed_amount), 0), COUNT(*)
      INTO imbalance, leg_count
      FROM ledger_entries
     WHERE transaction_id = txid;

    -- Every leg deleted: nothing left to balance.
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

-- Idempotency.
--
-- The composite primary key IS the concurrency control: a duplicate request
-- racing an in-flight one blocks on this key's index tuple until the owner
-- commits or aborts. Because the claim row and the ledger writes commit
-- together, there is no in-flight state to reconcile and no stale rows to
-- garbage-collect -- hence no 'state' column.
CREATE TABLE idempotency_keys (
    key             text        NOT NULL,
    endpoint        text        NOT NULL,
    request_hash    bytea       NOT NULL,
    response_status int,
    response_body   jsonb,
    transaction_id  uuid REFERENCES transactions(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (key, endpoint)
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- Transactional outbox.
CREATE TABLE outbox_events (
    id             uuid PRIMARY KEY,
    aggregate_type text          NOT NULL,
    aggregate_id   uuid          NOT NULL,
    event_type     text          NOT NULL,
    schema_version int           NOT NULL,
    payload        jsonb         NOT NULL,
    status         outbox_status NOT NULL DEFAULT 'pending',
    attempts       int           NOT NULL DEFAULT 0,
    last_error     text,
    available_at   timestamptz   NOT NULL DEFAULT now(),
    published_at   timestamptz,
    created_at     timestamptz   NOT NULL DEFAULT now()
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (available_at) WHERE status <> 'published';

CREATE TABLE outbox_deliveries (
    id           bigserial PRIMARY KEY,
    event_id     uuid        NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    succeeded    boolean     NOT NULL,
    trigger      text        NOT NULL,
    error        text
);

CREATE INDEX outbox_deliveries_event_id_idx ON outbox_deliveries (event_id);

-- Reconciliation.
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
    kind           text  NOT NULL,
    statement_ref  text,
    transaction_id uuid,
    details        jsonb NOT NULL
);

CREATE INDEX reconciliation_discrepancies_run_id_idx
    ON reconciliation_discrepancies (run_id);
