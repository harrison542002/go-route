package ports

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// RecordWriter persists a batch of decisions. Buffered sinks own the batching;
// implementations own the durability.
type RecordWriter interface {
	Write(ctx context.Context, batch []domains.RoutingDecision) error
}

// DecisionSink accepts audit records. Implementations must be safe for
// concurrent use: every request goroutine calls Record.
type DecisionSink interface {
	// Record accepts a decision. It MUST NOT block and MUST NOT fail.
	Record(d domains.RoutingDecision)

	// Flush blocks until buffered records are persisted. Shutdown only.
	Flush(ctx context.Context) error
}
