package bootstrap

import (
	"context"
	"fmt"

	adaptedpricing "github.com/harrison542002/go-route/internal/adapters/outbound/pricing"
	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/pricing"
)

type noopSink struct{}

func (noopSink) Record(domains.RoutingDecision) {}
func (noopSink) Flush(context.Context) error    { return nil }

type sinkBuilder struct {
	sink ports.DecisionSink
	mem  *sink.MemoryWriter
	err  error
}

func newSinkBuilder() *sinkBuilder {
	return &sinkBuilder{}
}

// withDestination picks where records ultimately land. It must be called
// first: every with* layer wraps whatever the destination produced.
func (b *sinkBuilder) withDestination(cfg config.Sink) *sinkBuilder {
	if b.err != nil {
		return b
	}

	switch cfg.Type {
	case "none":
		b.sink = noopSink{}

	case "log":
		b.sink = sink.NewBuffered(sink.SlogWriter{}, bufferedCfg(cfg))

	case "memory":
		b.mem = &sink.MemoryWriter{}
		b.sink = sink.NewBuffered(b.mem, bufferedCfg(cfg))

	default:
		// Unreachable: config.validate rejects unknown types at load.
		b.err = fmt.Errorf("sink: unsupported type %q", cfg.Type)
	}

	return b
}

// withPricing attaches cost to each record.
func (b *sinkBuilder) withPricing(cfg config.Pricing) *sinkBuilder {
	if b.err != nil || len(cfg.Table) == 0 {
		return b
	}

	table, err := adaptedpricing.NewTable(cfg.Table)
	if err != nil {
		b.err = fmt.Errorf("pricing table: %w", err)
		return b
	}

	b.sink = pricing.NewSink(pricing.New(table, cfg.CompareAgainst), b.sink)
	return b
}

// build returns the assembled sink and, where the destination is
func (b *sinkBuilder) build() (ports.DecisionSink, *sink.MemoryWriter, error) {
	if b.err != nil {
		return nil, nil, b.err
	}
	if b.sink == nil {
		return nil, nil, fmt.Errorf("sink: no destination configured")
	}
	return b.sink, b.mem, nil
}

func bufferedCfg(c config.Sink) sink.Config {
	return sink.Config{
		BufferSize:    c.BufferSize,
		BatchSize:     c.BatchSize,
		FlushInterval: c.FlushInterval,
	}
}
