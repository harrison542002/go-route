package bootstrap

import (
	"context"
	"fmt"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type NoopSink struct{}

func (NoopSink) Record(domains.RoutingDecision) {}
func (NoopSink) Flush(context.Context) error    { return nil }

func buildSink(cfg config.Sink) (ports.DecisionSink, *sink.MemoryWriter, error) {
	switch cfg.Type {
	case "none":
		return NoopSink{}, nil, nil

	case "log":
		return sink.NewBuffered(sink.SlogWriter{}, bufferedCfg(cfg)), nil, nil

	case "memory":
		mem := &sink.MemoryWriter{}
		return sink.NewBuffered(mem, bufferedCfg(cfg)), mem, nil

	default:
		// Unreachable: config.validate rejects unknown types at load.
		return nil, nil, fmt.Errorf("sink: unsupported type %q", cfg.Type)
	}
}

func bufferedCfg(c config.Sink) sink.Config {
	return sink.Config{
		BufferSize:    c.BufferSize,
		BatchSize:     c.BatchSize,
		FlushInterval: c.FlushInterval,
	}
}
