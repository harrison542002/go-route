package ports

import (
	"context"
	"errors"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var ErrRejected = errors.New("sink: batch rejected")

// RecordWriter persists a batch of decisions. Implementations must be idempotent
// per decision ID: the spool delivers at least once, and a crash between a commit
// and the spool forgetting the segment replays rows that were already written.
type RecordWriter interface {
	Write(ctx context.Context, batch []domains.RoutingDecision) error
}

// DecisionSink accepts audit records. Implementations must be safe for
// concurrent use: every request goroutine calls Record.
type DecisionSink interface {
	// Record accepts a decision. It MUST NOT fail and MUST NOT wait on the
	// database. It may wait on local storage, where the only alternative is
	// losing the record.
	Record(d domains.RoutingDecision)

	// Flush stops accepting records into the normal path and makes a best
	// effort to persist what it holds within ctx. Shutdown only.
	Flush(ctx context.Context) error
}
