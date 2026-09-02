// Command migrate applies or rolls back the ledger schema.
//
// Usage:
//
//	migrate up            apply all pending migrations
//	migrate down [n]      roll back the newest n migrations (default 1)
//	migrate version       print the current schema version
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate (up|down [n]|version)")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	switch args[0] {
	case "up":
		n, err := postgres.Up(ctx, conn)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d migration(s)\n", n)

	case "down":
		steps := 1
		if len(args) > 1 {
			if steps, err = strconv.Atoi(args[1]); err != nil {
				return fmt.Errorf("down: bad step count %q: %w", args[1], err)
			}
		}
		n, err := postgres.Down(ctx, conn, steps)
		if err != nil {
			return err
		}
		fmt.Printf("rolled back %d migration(s)\n", n)

	case "version":
		v, err := postgres.Version(ctx, conn)
		if err != nil {
			return err
		}
		fmt.Printf("schema version: %d\n", v)

	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
