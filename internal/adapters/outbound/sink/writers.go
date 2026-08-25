package sink

import (
	"context"
	"log/slog"
	"sync"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// SlogWriter emits records as structured logs.
type SlogWriter struct{}

func (SlogWriter) Write(_ context.Context, batch []domains.RoutingDecision) error {
	for _, d := range batch {
		slog.Info("decision",
			"id", d.ID.String(),
			"tenant", string(d.Tenant),
			"model", d.Request.RequestedModel,
			"reason", string(d.Ladder.Reason.Kind),
			"status", string(d.Outcome.Status),
			"attempts", len(d.Outcome.Attempts),
			"input_tokens", d.Outcome.Usage.Input,
			"cached_tokens", d.Outcome.Usage.CacheRead,
			"output_tokens", d.Outcome.Usage.Output,
			"ttft_ms", d.Outcome.TTFTMs,
			"total_ms", d.Outcome.TotalMs,
			"chosen", domains.Outcome.ChosenTarget(d.Outcome),
		)
	}
	return nil
}

// MemoryWriter retains records for tests.
type MemoryWriter struct {
	mu      sync.Mutex
	records []domains.RoutingDecision
}

func (m *MemoryWriter) Write(_ context.Context, batch []domains.RoutingDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, batch...)
	return nil
}

func (m *MemoryWriter) Records() []domains.RoutingDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domains.RoutingDecision(nil), m.records...)
}

// TODO: PostgresWriter Later
