package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/bootstrap"
	"github.com/harrison542002/go-route/internal/config"
)

const drainTimeout = 25 * time.Second

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe()
		},
	}
}

func runServe() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBuild()

	application, err := bootstrap.Build(buildCtx, cfg)
	if err != nil {
		return err
	}

	server := httpapi.NewServer(cfg.Listen, application.Handler)

	serverCtx, abortInFlight := context.WithCancel(context.Background())
	defer abortInFlight()
	server.BaseContext = func(net.Listener) context.Context { return serverCtx }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "models", modelNames(cfg))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		_ = application.Close()
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "drain_timeout", drainTimeout)
	}

	stop()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()

	if err := server.Shutdown(drainCtx); errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("drain window expired; cutting remaining streams")
	}
	abortInFlight()

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	if err := application.Sink.Flush(flushCtx); err != nil {
		slog.Error("sink flush failed; records lost", "err", err)
	}

	if err := application.Close(); err != nil {
		slog.Error("close failed", "err", err)
	}

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
