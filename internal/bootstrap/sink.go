package bootstrap

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	adaptedpricing "github.com/harrison542002/go-route/internal/adapters/outbound/pricing"
	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/pricing"
)

func buildSink(pool *pgxpool.Pool, cfg *config.Config, table ports.PricingTable) ports.DecisionSink {
	var s ports.DecisionSink = sink.NewBuffered(repositories.NewRecordWriter(pool), sink.Config{
		BufferSize:    cfg.Sink.BufferSize,
		BatchSize:     cfg.Sink.BatchSize,
		FlushInterval: cfg.Sink.FlushInterval,
	})

	if table != nil {
		s = pricing.NewSink(pricing.New(table, cfg.Pricing.CompareAgainst), s)
	}

	return s
}

func buildPricingTable(cfg *config.Config) (ports.PricingTable, error) {
	if len(cfg.Pricing.Table) == 0 {
		return nil, nil
	}
	table, err := adaptedpricing.NewTable(cfg.Pricing.Table)
	if err != nil {
		return nil, fmt.Errorf("pricing table: %w", err)
	}
	return table, nil
}
