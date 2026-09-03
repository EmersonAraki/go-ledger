// Command relay publishes outbox events.
//
// The API already runs this loop in-process, so this binary is only needed when
// delivery should scale separately from request handling. Several instances may
// run alongside the API without coordination: the dispatcher claims rows with
// FOR UPDATE SKIP LOCKED, so instances take disjoint batches instead of
// contending for the same ones.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EmersonAraki/go-ledger/internal/config"
	"github.com/EmersonAraki/go-ledger/internal/outbox"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	relay := outbox.NewRelay(postgres.NewStore(pool), outbox.LogPublisher{}, outbox.RelayOptions{})
	return relay.Run(ctx)
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
