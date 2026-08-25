// Package boostrap is the composition root: the single place that decides
// which concrete adapters satisfy which ports.
package boostrap

import (
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
	"github.com/harrison542002/go-route/internal/usecases/routing"
)

type App struct {
	Handler *httpapi.Handler
	// Sink    ports.DecisionSink // TODO: add decision sink
	Close func() error
}

func Build(cfg *config.Config) (*App, error) {
	providers, err := buildProviders(cfg) // the only oaicompat import
	if err != nil {
		return nil, err
	}

	table, resolver, err := routing.FromConfig(cfg, providers)
	if err != nil {
		return nil, err
	}

	return &App{
		Handler: httpapi.NewHandler(table, resolver, dispatch.New(time.Now), time.Now),
		Close:   func() error { return nil },
	}, nil
}
