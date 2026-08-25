package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/boostrap"
	"github.com/harrison542002/go-route/internal/config"
)

// Must be shorter than the orchestrator's grace period, or the process is
// SIGKILLed mid-drain and the wait achieves nothing. Kubernetes defaults
// terminationGracePeriodSeconds to 30.
const drainTimeout = 25 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "configs/go-route.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	application, err := boostrap.Build(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if application.Close != nil {
			if err := application.Close(); err != nil {
				slog.Error("close failed", "err", err)
			}
		}
	}()

	server := httpapi.NewServer(cfg.Listen, application.Handler)

	// Every request context derives from this, so cancelling it after the
	// drain window aborts stragglers rather than hanging forever.
	serverCtx, abortInFlight := context.WithCancel(context.Background())
	defer abortInFlight()
	server.BaseContext = func(net.Listener) context.Context { return serverCtx }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "models", modelNames(cfg))

		// ErrServerClosed is the normal result of Shutdown, not a failure.
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "drain_timeout", drainTimeout)
	}

	// Restore default signal handling: a second Ctrl-C now exits at once,
	// for an operator who does not want to wait out the drain.
	stop()

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Stops accepting, then waits for in-flight requests. For a streaming
	// proxy those are generations, not milliseconds.
	if err := server.Shutdown(drainCtx); errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("drain window expired; cutting remaining streams")
	}
	abortInFlight()

	// TODO(milestone-1): sink.Flush belongs HERE — after Shutdown, so
	// records for in-flight requests are complete, and before exit.

	slog.Info("shutdown complete")
	return nil
}

func modelNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
