package bootstrap

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	adaptedpricing "github.com/harrison542002/go-route/internal/adapters/outbound/pricing"
	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/pricing"
)

// buildSink assembles the record pipeline: a buffered queue in front of
// the Postgres writer, optionally priced on the way through.
//
// There is one destination because there is one database. go-route
// cannot authenticate a request without api_keys, so a deployment
// without Postgres cannot serve traffic at all -- which made the old
// log and none sinks a choice between "record this" and "serve requests
// you cannot attribute". Records go to the database or the gateway does
// not start.
func buildSink(pool *pgxpool.Pool, cfg *config.Config) (ports.DecisionSink, error) {
	var s ports.DecisionSink = sink.NewBuffered(sink.NewPostgresWriter(pool), sink.Config{
		BufferSize:    cfg.Sink.BufferSize,
		BatchSize:     cfg.Sink.BatchSize,
		FlushInterval: cfg.Sink.FlushInterval,
	})

	if len(cfg.Pricing.Table) > 0 {
		table, err := adaptedpricing.NewTable(cfg.Pricing.Table)
		if err != nil {
			return nil, fmt.Errorf("pricing table: %w", err)
		}
		s = pricing.NewSink(pricing.New(table, cfg.Pricing.CompareAgainst), s)
	}

	return s, nil
}
