package sink

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/harrison542002/go-route/internal/core/domains"
)

type SlogWriter struct{}

func (SlogWriter) Write(_ context.Context, batch []domains.RoutingDecision) error {
	for _, d := range batch {
		attrs := []any{
			"id", d.ID.String(),
			"model", d.Request.RequestedModel,
			"chosen", d.Outcome.ChosenTarget(),
			"status", string(d.Outcome.Status),
			"trail", attemptTrail(d.Outcome),
			"input_tokens", d.Outcome.Usage.Input,
			"cached_tokens", d.Outcome.Usage.CacheRead,
			"output_tokens", d.Outcome.Usage.Output,
			"ttft_ms", d.Outcome.TTFTMs,
			"total_ms", d.Outcome.TotalMs,
		}

		if d.Cost == nil {
			attrs = append(attrs, "cost", "unpriced")
		} else {
			attrs = append(attrs,
				"cost", d.Cost.Actual.String(),
				"price_table", d.Cost.PriceTableVersion,
			)
			for _, cf := range d.Cost.Counterfactuals {
				attrs = append(attrs, "vs_"+cf.Target, cf.Cost.String())
			}
			if len(d.Cost.UnpricedComparisons) > 0 {
				attrs = append(attrs, "unpriced_comparisons", d.Cost.UnpricedComparisons)
			}
		}

		slog.Info("decision", attrs...)
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

func attemptTrail(o domains.Outcome) string {
	parts := make([]string, 0, len(o.Attempts))
	for _, a := range o.Attempts {
		if a.Failure == nil {
			parts = append(parts, a.Target+":ok")
			continue
		}
		parts = append(parts, a.Target+":"+a.Failure.Kind)
	}
	return strings.Join(parts, " ")
}

// TODO: PostgresWriter Later
