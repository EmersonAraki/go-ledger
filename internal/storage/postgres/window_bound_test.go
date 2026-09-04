package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// windowFixture writes n two-leg transactions and n legless ones onto one fixed
// day, without going through the ledger service: this is about what the queries
// fetch, not about how the rows got there.
func windowFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) (time.Time, time.Time) {
	t.Helper()

	day := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	debit := newAccount(ctx, t, pool, "wb-debit", 0, true)
	credit := newAccount(ctx, t, pool, "wb-credit", 0, true)

	// One transaction for the whole fixture: the zero-sum trigger is deferred to
	// COMMIT, so a transaction's two legs have to land together.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	for i := range n {
		at := day.Add(time.Duration(i) * time.Minute)

		balanced := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO transactions (id, currency, created_at) VALUES ($1, 'BRL', $2)`,
			balanced, at); err != nil {
			t.Fatalf("insert transaction %d: %v", i, err)
		}
		for _, leg := range []struct {
			account   uuid.UUID
			direction string
		}{{debit, "debit"}, {credit, "credit"}} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger_entries
				    (id, transaction_id, account_id, direction, amount, currency, created_at)
				VALUES ($1, $2, $3, $4, 10, 'BRL', $5)`,
				uuid.New(), balanced, leg.account, leg.direction, at); err != nil {
				t.Fatalf("insert %s leg %d: %v", leg.direction, i, err)
			}
		}

		// A legless transaction, so the integrity scan has something it could
		// find at every position in the window too.
		if _, err := tx.Exec(ctx,
			`INSERT INTO transactions (id, currency, created_at) VALUES ($1, 'BRL', $2)`,
			uuid.New(), at); err != nil {
			t.Fatalf("insert legless transaction %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return day, day.Add(24 * time.Hour)
}

// Both window queries must stop at their cap in the DATABASE, not merely have
// their results sliced afterwards. Deleting either SQL LIMIT leaves every other
// test in this package green, so this is the only thing standing between the
// bound and a silent full-window scan.
func TestWindowQueriesNeverFetchMoreThanTheCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.Pool(t)

	const windowRows = 40
	start, end := windowFixture(ctx, t, pool, windowRows)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// A cap far below what the window holds.
	const limit = 5

	_, fetched, err := postgres.LoadLedgerWindow(ctx, tx, &start, &end, time.Hour, limit)
	if err != nil {
		t.Fatalf("load ledger window: %v", err)
	}
	if fetched != limit+1 {
		t.Errorf("ledger window fetched %d rows for a cap of %d, want %d (the cap plus "+
			"the one probe row that detects it biting) -- the window holds %d, so a "+
			"larger number means the SQL LIMIT is not bounding the database",
			fetched, limit, limit+1, windowRows)
	}

	_, examined, err := postgres.LoadUnreconcilable(ctx, tx, &start, &end, limit)
	if err != nil {
		t.Fatalf("load unreconcilable: %v", err)
	}
	if examined != limit+1 {
		t.Errorf("integrity scan examined %d transactions for a cap of %d, want %d -- "+
			"the window holds %d", examined, limit, limit+1, windowRows*2)
	}
}
