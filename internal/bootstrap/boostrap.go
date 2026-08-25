// Package boostrap is the composition root: the single place that decides
// which concrete adapters satisfy which ports.
package bootstrap

import (
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
	"github.com/harrison542002/go-route/internal/usecases/routing"
)

type App struct {
	Handler *httpapi.Handler
	Sink    ports.DecisionSink
	Memory  *sink.MemoryWriter
	Close   func() error
}

func Build(cfg *config.Config) (*App, error) {
	providers, err := buildProviders(cfg)
	if err != nil {
		return nil, err
	}

	table, resolver, err := routing.FromConfig(cfg, providers)
	if err != nil {
		return nil, err
	}

	s, mem, err := buildSink(cfg.Sink)
	if err != nil {
		return nil, err
	}

	return &App{
		Handler: httpapi.NewHandler(table, resolver, dispatch.New(time.Now), s, time.Now),
		Memory:  mem,
		Sink:    s,
		Close:   func() error { return nil },
	}, nil
}
