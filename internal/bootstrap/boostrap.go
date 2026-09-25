package bootstrap

import (
	"context"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/proxyapi"
	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/drivers/postgresql"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
	"github.com/harrison542002/go-route/internal/usecases/routing"
)

type App struct {
	Handler *proxyapi.Handler
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

	pool, err := postgresql.Connect(ctx, cfg.Sink.DSN)
	if err != nil {
		return nil, err
	}

	partitions := postgresql.NewPartitions(pool)
	if err := partitions.Ensure(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	//nolint:contextcheck // maintenance outlives any caller's context by design
	partitions.Start()

	prices, err := buildPricingTable(cfg)
	if err != nil {
		_ = partitions.Stop()
		pool.Close()
		return nil, err
	}

	//nolint:contextcheck // the sink's flush loop is deliberately detached; records must survive the request they describe
	builtSink := buildSink(pool, cfg, prices)

	enforcer, closeQuota := buildQuota(ctx, pool, cfg, prices)

	return &App{
		Handler: proxyapi.NewHandler(
			table, resolver, dispatch.New(time.Now), builtSink, enforcer, time.Now),
		Auth: repositories.NewAuth(pool),
		Sink: builtSink,
		Close: func() error {
			err := closeQuota()
			if perr := partitions.Stop(); err == nil {
				err = perr
			}
			pool.Close()
			return err
		},
	}, nil
}
