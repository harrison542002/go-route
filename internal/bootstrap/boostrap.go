// Package bootstrap is the composition root: the single place that decides
// which concrete adapters satisfy which ports.
package bootstrap

import (
	"context"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
	"github.com/harrison542002/go-route/internal/usecases/routing"
)

type App struct {
	Handler *httpapi.Handler
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

	builtSink, err := newSinkBuilder().
		withDestination(ctx, cfg.Sink).
		withPricing(cfg.Pricing).
		build()

	if err != nil {
		return nil, err
	}

	return &App{
		Handler: httpapi.NewHandler(table, resolver, dispatch.New(time.Now), builtSink.Sink, time.Now),
		Sink:    builtSink.Sink,
		Close:   builtSink.Close,
	}, nil
}
