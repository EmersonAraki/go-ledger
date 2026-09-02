package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID guards the migration sequence so two processes booting at once
// cannot apply the same migration twice. The value is arbitrary but must be stable.
const migrationLockID int64 = 8022026

// Migration is one versioned schema change, loaded from the embedded FS.
type Migration struct {
	Version  int64
	Name     string
	Up       string
	Down     string
	Checksum string
}

// LoadMigrations parses the embedded migrations directory. Files are named
// NNNN_name.up.sql and NNNN_name.down.sql; a down file is optional.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		base := strings.TrimSuffix(e.Name(), ".sql")
		direction := path.Ext(base) // ".up" or ".down"
		stem := strings.TrimSuffix(base, direction)

		idx := strings.Index(stem, "_")
		if idx <= 0 {
			return nil, fmt.Errorf("migration %q: expected NNNN_name.(up|down).sql", e.Name())
		}
		version, err := strconv.ParseInt(stem[:idx], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", e.Name(), err)
		}

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}

		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: stem[idx+1:]}
			byVersion[version] = m
		}

		switch direction {
		case ".up":
			m.Up = string(body)
		case ".down":
			m.Down = string(body)
		default:
			return nil, fmt.Errorf("migration %q: expected .up.sql or .down.sql", e.Name())
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("migration %d (%s): missing .up.sql", m.Version, m.Name)
		}
		sum := sha256.Sum256([]byte(m.Up))
		m.Checksum = hex.EncodeToString(sum[:])
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ensureMigrationTable creates the bookkeeping table. It is itself idempotent
// and deliberately not part of the versioned sequence.
func ensureMigrationTable(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint      PRIMARY KEY,
			name       text        NOT NULL,
			checksum   text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	return err
}

// appliedMigrations returns version -> checksum for everything already applied.
func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := map[int64]string{}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		applied[v] = sum
	}
	return applied, rows.Err()
}

// Up applies every pending migration in version order, each in its own
// transaction, and verifies that already-applied migrations have not been
// edited since. It returns the number of migrations applied.
func Up(ctx context.Context, conn *pgx.Conn) (int, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return 0, err
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if err := ensureMigrationTable(ctx, conn); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}

	count := 0
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			// An edited migration means the database and the code disagree about
			// what version N is. Refuse rather than silently diverge.
			if sum != m.Checksum {
				return count, fmt.Errorf(
					"migration %d (%s) was modified after being applied (checksum %s != %s)",
					m.Version, m.Name, sum, m.Checksum)
			}
			continue
		}

		if err := applyOne(ctx, conn, m, true); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Down rolls back the newest n applied migrations, most recent first.
// Down migrations exist for local development; CI is forward-only.
func Down(ctx context.Context, conn *pgx.Conn, n int) (int, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return 0, err
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if err := ensureMigrationTable(ctx, conn); err != nil {
		return 0, err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}

	count := 0
	for i := len(migrations) - 1; i >= 0 && count < n; i-- {
		m := migrations[i]
		if _, ok := applied[m.Version]; !ok {
			continue
		}
		if m.Down == "" {
			return count, fmt.Errorf("migration %d (%s) has no down migration", m.Version, m.Name)
		}
		if err := applyOne(ctx, conn, m, false); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// applyOne runs a single migration and its bookkeeping in one transaction, so a
// failure leaves neither the schema nor schema_migrations half-updated.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration, up bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	body, direction := m.Up, "up"
	if !up {
		body, direction = m.Down, "down"
	}

	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("migration %d (%s) %s: %w", m.Version, m.Name, direction, err)
	}

	if up {
		_, err = tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Checksum)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version)
	}
	if err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}

	return tx.Commit(ctx)
}

// Version reports the highest applied migration version, or 0 when none have run.
func Version(ctx context.Context, conn *pgx.Conn) (int64, error) {
	if err := ensureMigrationTable(ctx, conn); err != nil {
		return 0, err
	}
	var v *int64
	if err := conn.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}
