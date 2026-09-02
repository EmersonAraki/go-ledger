// Package pgtest gives integration tests a real, isolated PostgreSQL schema.
//
// The correctness claims in this project are database claims -- unique
// constraints, deferred triggers, row locks, snapshot behaviour. A mocked
// database would test none of them, so tests run against real PostgreSQL.
//
// Set TEST_DATABASE_URL to point at a scratch database; without it, integration
// tests skip rather than fail, so `go test ./...` still works with no services
// running.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// EnvURL is the environment variable holding the test database connection string.
const EnvURL = "TEST_DATABASE_URL"

// Pool returns a migrated, isolated pool for a single test.
//
// Each test gets its own PostgreSQL schema, so tests may run in parallel and
// cannot see each other's rows. The schema is dropped when the test ends.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("%s not set; skipping integration test", EnvURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	schema := "test_" + randomSuffix(t)

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}
	// Pin every connection in this pool to the test's own schema, so unqualified
	// DDL and DML in the migrations land there.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()

		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, err := pgx.Connect(cleanupCtx, url)
		if err != nil {
			t.Logf("cleanup: connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(context.WithoutCancel(cleanupCtx)) }()

		drop := fmt.Sprintf("DROP SCHEMA %s CASCADE", pgx.Identifier{schema}.Sanitize())
		if _, err := conn.Exec(cleanupCtx, drop); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
	})

	migrate(ctx, t, pool)
	return pool
}

// migrate applies the full schema to the test's pool.
func migrate(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection for migration: %v", err)
	}
	defer conn.Release()

	if _, err := postgres.Up(ctx, conn.Conn()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}
