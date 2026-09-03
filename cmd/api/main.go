// Command api serves the sumzero ledger HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EmersonAraki/go-ledger/internal/config"
	"github.com/EmersonAraki/go-ledger/internal/httpapi"
	"github.com/EmersonAraki/go-ledger/internal/ledger"
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

	// Cancelled on SIGINT/SIGTERM, which begins graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := postgres.NewStore(pool)
	publisher := outbox.LogPublisher{}

	// The relay runs in-process by default so a single binary is a complete,
	// working system. cmd/relay runs the same loop standalone when delivery
	// needs to scale independently of the API; SKIP LOCKED means both can run at
	// once without coordination.
	relay := outbox.NewRelay(store, publisher, outbox.RelayOptions{})
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		if err := relay.Run(ctx); err != nil {
			slog.Error("outbox relay exited", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewServer(pool, ledger.NewService(store), store, publisher).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// Bounds reading the request body. This must grow before the
		// reconciliation endpoint starts accepting large CSV uploads.
		ReadTimeout: 15 * time.Second,
		// Must exceed the handler timeout, so a timing-out handler still has an
		// open connection to write its error response on.
		WriteTimeout: httpapi.RequestTimeout + 5*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)
	}

	// Stop accepting new connections and let in-flight requests finish, so a
	// deploy cannot tear down a half-committed ledger write.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	// The relay stops on the same cancelled context; wait for its current batch
	// so a shutdown cannot abandon a transaction mid-publish.
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		slog.Warn("outbox relay did not stop before the shutdown deadline")
	}

	slog.Info("shutdown complete")
	return nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
