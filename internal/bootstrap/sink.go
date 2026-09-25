package bootstrap

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	adaptedpricing "github.com/harrison542002/go-route/internal/adapters/outbound/pricing"
	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/pricing"
)

// buildSink assembles the record pipeline: pricing first, so the spool holds the
// priced record and a replay never reprices it against a table that has since
// changed; then the on-disk spool, which ships to the Postgres record writer.
func buildSink(pool *pgxpool.Pool, cfg *config.Config, table ports.PricingTable) (ports.DecisionSink, error) {
	spooled, err := sink.NewSpooled(repositories.NewRecordWriter(pool), sink.Config{
		Dir:             cfg.Sink.SpoolDir,
		SegmentMaxBytes: cfg.Sink.SegmentMaxBytes,
		Sync:            sink.SyncMode(cfg.Sink.Sync),
		SyncInterval:    cfg.Sink.SyncInterval,
		MaxSpoolBytes:   cfg.Sink.MaxSpoolBytes,
		BufferSize:      cfg.Sink.BufferSize,
		BatchSize:       cfg.Sink.BatchSize,
		FlushInterval:   cfg.Sink.FlushInterval,
	}, time.Now)
	if err != nil {
		return nil, err
	}

	var s ports.DecisionSink = spooled
	if table != nil {
		s = pricing.NewSink(pricing.New(table, cfg.Pricing.CompareAgainst), s)
	}

	return s, nil
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
