// Command zirnis runs the Zirnis payments and core-banking API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo; COB scheduling needs Africa/Nairobi

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/internal/workers"
)

func main() {
	// `zirnis healthcheck` probes the running API; the distroless image has no curl or wget.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// `zirnis migrate` migrates and bootstraps the database, then exits: run it once before a
	// rollout (a Kubernetes Job), so instances start against a ready schema.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrate(logger); err != nil {
			logger.Error("migration failed", "error", err)
			os.Exit(1)
		}
		return
	}
	// `zirnis partition` converts the journal lines and the audit trail to monthly partitions:
	// PostgreSQL only, once, with the API stopped (ADR 0029).
	if len(os.Args) > 1 && os.Args[1] == "partition" {
		if err := partition(logger); err != nil {
			logger.Error("partitioning failed; nothing changed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("zirnis stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sys, err := system.Build(ctx, cfg, clock.System{}, logger)
	if err != nil {
		return err
	}
	defer sys.Close()
	registerExtensions(sys)

	waitInstance := workers.Instance(ctx, sys.App, logger)
	waitWorkers := func() {}
	if cfg.WorkersEnabled {
		location, err := time.LoadLocation(cfg.TimeZone)
		if err != nil {
			return err
		}
		waitWorkers = workers.Start(ctx, sys.App, sys.Store, workers.Options{OutboxInterval: cfg.OutboxInterval, OperationsInterval: cfg.OperationsInterval,
			AutoCobAt: cfg.AutoCobAt, Location: location, StaleAfter: cfg.StaleAfter, RabbitMQURL: cfg.RabbitMQURL}, logger)
	}

	servers := []*http.Server{newServer(cfg.Addr, sys.Handler())}
	if cfg.EdgeAddr != "" {
		servers = append(servers, newServer(cfg.EdgeAddr, sys.EdgeHandler()))
	}
	errs := make(chan error, len(servers))
	for i, server := range servers {
		go func() {
			if i == 0 {
				logger.Info("zirnis listening", "addr", cfg.Addr, "environment", cfg.Environment, "sandbox", cfg.SandboxEnabled(), "datasource", cfg.Datasource)
			} else {
				logger.Info("payer edge listening", "addr", cfg.EdgeAddr, "publicUrl", cfg.PublicURL)
			}
			errs <- server.ListenAndServe()
		}()
	}

	var failed error
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			failed = err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}
	// Drain: readiness fails first, so load balancers stop routing here; then the servers stop
	// accepting and finish the requests in flight; then the workers finish their cycle, before
	// the database closes.
	sys.App.Draining.Store(true)
	if cfg.ShutdownDrain > 0 {
		logger.Info("draining before shutdown", "for", cfg.ShutdownDrain.String())
		time.Sleep(cfg.ShutdownDrain)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	for _, server := range servers {
		failed = errors.Join(failed, server.Shutdown(shutdown))
	}
	stop()
	waitWorkers()
	waitInstance()
	// Close of business no longer waits for this instance.
	failed = errors.Join(failed, sys.App.Deregister(context.Background()))
	logger.Info("stopped")
	return failed
}

func migrate(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.WorkersEnabled = false
	sys, err := system.Build(context.Background(), cfg, clock.System{}, logger) // migrates and bootstraps
	if err != nil {
		return err
	}
	logger.Info("database migrated and bootstrapped")
	return sys.Close()
}

func partition(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.WorkersEnabled = false
	sys, err := system.Build(context.Background(), cfg, clock.System{}, logger)
	if err != nil {
		return err
	}
	defer sys.Close()
	logger.Info("partitioning journal_lines and audit_events by month; this copies both tables")
	started := time.Now()
	if err := sys.Store.Partition(context.Background(), time.Now()); err != nil {
		return err
	}
	for _, table := range []string{"journal_lines", "audit_events"} {
		parts, err := sys.Store.Partitions(context.Background(), table)
		if err != nil {
			return err
		}
		logger.Info("partitioned", "table", table, "partitions", len(parts))
	}
	logger.Info("done", "took", time.Since(started).Round(time.Second).String())
	return nil
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func healthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health/live") // #nosec G107 -- loopback probe of our own port
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
