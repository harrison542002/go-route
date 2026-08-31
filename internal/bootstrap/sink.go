package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

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
	sink    ports.DecisionSink
	closers []func() error
	err     error
}

type builtSink struct {
	Sink  ports.DecisionSink
	Close func() error
}

func newSinkBuilder() *sinkBuilder {
	return &sinkBuilder{}
}

// withDestination picks where records ultimately land. It must be called
// first: every with* layer wraps whatever the destination produced.
func (b *sinkBuilder) withDestination(ctx context.Context, cfg config.Sink) *sinkBuilder {
	if b.err != nil {
		return b
	}

	switch cfg.Type {
	case string(ports.NONE):
		b.sink = noopSink{}

	case string(ports.LOG):
		//nolint:contextcheck // background sink loop is intentionally detached from caller ctx
		b.sink = sink.NewBuffered(sink.SlogWriter{}, bufferedCfg(cfg))

	case string(ports.POSTGRES):
		w, err := sink.NewPostgresWriter(ctx, cfg.DSN)
		if err != nil {
			b.err = err
			return b
		}
		b.closers = append(b.closers, w.Close)

		//nolint:contextcheck // background sink loop is intentionally detached from caller ctx
		b.sink = sink.NewBuffered(w, bufferedCfg(cfg))

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

func (b *sinkBuilder) build() (*builtSink, error) {
	if b.err != nil {
		if cerr := b.closeAll(); cerr != nil {
			slog.Error("cleanup after sink build failure", "err", cerr)
		}
		return nil, b.err
	}
	if b.sink == nil {
		return nil, fmt.Errorf("sink: no destination configured")
	}
	return &builtSink{
		Sink:  b.sink,
		Close: b.closeAll,
	}, nil
}

func (b *sinkBuilder) closeAll() error {
	var firstErr error
	for i := len(b.closers) - 1; i >= 0; i-- {
		if err := b.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func bufferedCfg(c config.Sink) sink.Config {
	return sink.Config{
		BufferSize:    c.BufferSize,
		BatchSize:     c.BatchSize,
		FlushInterval: c.FlushInterval,
	}
}
