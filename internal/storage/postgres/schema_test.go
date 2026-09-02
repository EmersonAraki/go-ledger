package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// newAccount inserts an account and returns its id.
func newAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, balance int64, allowNegative bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, name, kind, currency, balance, allow_negative_balance)
		VALUES ($1, $2, 'asset', 'BRL', $3, $4)`,
		id, name, balance, allowNegative)
	if err != nil {
		t.Fatalf("insert account %s: %v", name, err)
	}
	return id
}

// postEntries writes a transaction and its legs inside one database transaction,
// mirroring what the ledger service will do. Returns the COMMIT error, which is
// where the deferred zero-sum trigger reports failure.
func postEntries(ctx context.Context, t *testing.T, pool *pgxpool.Pool, legs []leg) error {
	t.Helper()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	txID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO transactions (id, currency) VALUES ($1, 'BRL')`, txID); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	for _, l := range legs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (id, transaction_id, account_id, direction, amount, currency)
			VALUES ($1, $2, $3, $4, $5, 'BRL')`,
			uuid.New(), txID, l.account, l.direction, l.amount); err != nil {
			// An insert may fail immediately (e.g. amount <= 0); surface it as-is.
			return err
		}
	}
	return tx.Commit(ctx)
}

type leg struct {
	account   uuid.UUID
	direction string
	amount    int64
}

func TestBalancedTransactionCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	alice := newAccount(ctx, t, pool, "alice", 1000, false)
	bob := newAccount(ctx, t, pool, "bob", 0, false)

	err := postEntries(ctx, t, pool, []leg{
		{bob, "debit", 300},
		{alice, "credit", 300},
	})
	if err != nil {
		t.Fatalf("balanced transaction should commit, got: %v", err)
	}

	var sum int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(signed_amount), 0) FROM ledger_entries`).Scan(&sum); err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if sum != 0 {
		t.Errorf("ledger does not sum to zero: got %d", sum)
	}
}

func TestUnbalancedTransactionRejectedAtCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	alice := newAccount(ctx, t, pool, "alice", 1000, false)
	bob := newAccount(ctx, t, pool, "bob", 0, false)

	// Legs differ by 1 minor unit.
	err := postEntries(ctx, t, pool, []leg{
		{bob, "debit", 300},
		{alice, "credit", 299},
	})
	if err == nil {
		t.Fatal("expected unbalanced transaction to be rejected, but it committed")
	}
	assertCheckViolation(t, err)

	assertNoEntries(ctx, t, pool)
}

func TestSingleLegTransactionRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	bob := newAccount(ctx, t, pool, "bob", 0, false)

	err := postEntries(ctx, t, pool, []leg{{bob, "debit", 50}})
	if err == nil {
		t.Fatal("expected single-leg transaction to be rejected, but it committed")
	}
	assertCheckViolation(t, err)

	assertNoEntries(ctx, t, pool)
}

func TestBalanceFloorRejectsOverdraft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	alice := newAccount(ctx, t, pool, "alice", 100, false)

	_, err := pool.Exec(ctx,
		`UPDATE accounts SET balance = balance - 500 WHERE id = $1`, alice)
	if err == nil {
		t.Fatal("expected overdraft to violate accounts_balance_floor")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "accounts_balance_floor" {
		t.Fatalf("expected accounts_balance_floor violation, got: %v", err)
	}
}

func TestSystemAccountMayGoNegative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	// Money enters the ledger from a system account, which must be allowed to
	// carry a negative balance for the zero-sum invariant to hold globally.
	external := newAccount(ctx, t, pool, "external", 0, true)

	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET balance = balance - 500 WHERE id = $1`, external); err != nil {
		t.Fatalf("system account should be allowed to go negative: %v", err)
	}

	var balance int64
	if err := pool.QueryRow(ctx,
		`SELECT balance FROM accounts WHERE id = $1`, external).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != -500 {
		t.Errorf("balance = %d, want -500", balance)
	}
}

func TestNonPositiveAmountRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	alice := newAccount(ctx, t, pool, "alice", 1000, false)
	bob := newAccount(ctx, t, pool, "bob", 0, false)

	err := postEntries(ctx, t, pool, []leg{
		{bob, "debit", 0},
		{alice, "credit", 0},
	})
	if err == nil {
		t.Fatal("expected zero-amount entry to be rejected")
	}
}

func TestExternalRefIsUniqueButNullable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	insert := func(ref *string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO transactions (id, currency, external_ref) VALUES ($1, 'BRL', $2)`,
			uuid.New(), ref)
		return err
	}

	// Many transactions may have no external reference.
	if err := insert(nil); err != nil {
		t.Fatalf("first null external_ref: %v", err)
	}
	if err := insert(nil); err != nil {
		t.Fatalf("second null external_ref should be allowed: %v", err)
	}

	ref := "TRX-1"
	if err := insert(&ref); err != nil {
		t.Fatalf("first TRX-1: %v", err)
	}
	if err := insert(&ref); err == nil {
		t.Fatal("expected duplicate external_ref to be rejected")
	}
}

func TestMigrationsAreIdempotentAndReversible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t) // already migrated up

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Running up again must be a no-op, not an error.
	n, err := postgres.Up(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("second migrate up: %v", err)
	}
	if n != 0 {
		t.Errorf("second migrate up applied %d migrations, want 0", n)
	}

	// Down then up must leave a working schema behind.
	if _, err := postgres.Down(ctx, conn.Conn(), 1); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if _, err := postgres.Up(ctx, conn.Conn()); err != nil {
		t.Fatalf("migrate up after down: %v", err)
	}

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('ledger_entries') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !exists {
		t.Error("ledger_entries missing after down/up cycle")
	}
}

func assertNoEntries(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if count != 0 {
		t.Errorf("rejected transaction left %d entries behind", count)
	}
}

func assertCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	// 23514 = check_violation, raised by assert_transaction_balanced.
	if pgErr.Code != "23514" {
		t.Errorf("expected check_violation (23514), got %s: %s", pgErr.Code, pgErr.Message)
	}
}
