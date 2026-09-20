// Package bootstrap is the composition root: the single place that decides
// which concrete adapters satisfy which ports.
package bootstrap

import (
	"context"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
	"github.com/harrison542002/go-route/internal/usecases/routing"
)

type App struct {
	Handler *httpapi.Handler
	Auth    ports.Authenticator
	Sink    ports.DecisionSink
	Close   func() error
}

func Build(ctx context.Context, cfg *config.Config) (*App, error) {
	providers, err := buildProviders(cfg)
	if err != nil {
		return nil, err
	}

	table, resolver, err := routing.FromConfig(cfg, providers)
	if err != nil {
		return nil, err
	}

	// One pool for the whole process, shared by the writer, the
	// authenticator and partition maintenance.
	pool, err := postgresql.Connect(ctx, cfg.Sink.DSN)
	if err != nil {
		return nil, err
	}

	// Ensure once here so a database with no partitions fails startup,
	// rather than being discovered at the first write with nowhere to
	// write to. The ticker keeps coverage ahead of the calendar
	// afterwards, so uptime never outruns it.
	partitions := postgresql.NewPartitions(pool)
	if err := partitions.Ensure(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	//nolint:contextcheck // maintenance outlives any caller's context by design
	partitions.Start()

	//nolint:contextcheck // the sink's flush loop is deliberately detached; records must survive the request they describe
	builtSink, err := buildSink(pool, cfg)
	if err != nil {
		_ = partitions.Stop()
		pool.Close()
		return nil, err
	}

	return &App{
		Handler: httpapi.NewHandler(
			table, resolver, dispatch.New(time.Now), builtSink, time.Now),
		Auth: postgresql.NewAuth(pool),
		Sink: builtSink,
		Close: func() error {
			// Callers flush the sink before calling this, so the pool
			// closing last is what keeps that final batch writable.
			err := partitions.Stop()
			pool.Close()
			return err
		},
	}, nil
}
