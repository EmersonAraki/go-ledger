// Command driftsweep checks every account's cached balance against the sum of
// its entries and exits.
//
// This is the whole-ledger half of reconciliation. It is a separate binary, run
// on a schedule, because a derived balance is the sum of an account's entire
// history: there is no predicate that bounds it, so it must not sit on a request
// path where a single uploaded CSV row would pay for a full scan. Run it from
// cron as often as the ledger's size affords.
//
// Exit status is 0 when every account agreed, 2 when drift was found, and 1 on
// error -- so a cron wrapper can alert on drift without parsing the output.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EmersonAraki/go-ledger/internal/config"
	"github.com/EmersonAraki/go-ledger/internal/reconcile"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// Exit codes beyond the usual 0 and 1.
const exitDriftFound = 2

func main() {
	found, err := run()
	if err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
	if found {
		os.Exit(exitDriftFound)
	}
}

func run() (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	setupLogging(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return false, err
	}
	defer pool.Close()

	store := postgres.NewStore(pool)

	report, err := store.SweepBalanceDrift(ctx, reconcile.DefaultDriftPageSize)
	if err != nil {
		return false, err
	}
	if report == nil {
		slog.InfoContext(ctx, "balance drift sweep clean")
		return false, nil
	}

	// Log every drifting account, not just the count: the run is persisted and
	// readable over the API, but an operator woken by this needs the accounts.
	drifted := 0
	for _, d := range report.Discrepancies {
		if d.Kind != reconcile.KindBalanceDrift {
			continue
		}
		drifted++
		slog.ErrorContext(ctx, "balance drift",
			"account_id", d.Details["account_id"],
			"account_name", d.Details["account_name"],
			"cached_balance", d.Details["cached_balance"],
			"derived_balance", d.Details["derived_balance"],
			"difference", d.Details["difference"])
	}
	slog.ErrorContext(ctx, "balance drift sweep found drift",
		"run_id", report.ID, "accounts_checked", report.StatementRows,
		"accounts_drifted", drifted)
	return true, nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
