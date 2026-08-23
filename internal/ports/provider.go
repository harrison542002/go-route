//go:generate mockgen -source=provider.go -destination=mocks/provider_mock.go -package=mocks

package ports

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// StreamReader yields events from one upstream response.
// Not safe for concurrent use; each response is read by one goroutine.
type Provider interface {
	Stream(ctx context.Context, req *ProviderRequest) (StreamReader, error)
	Name() string
}

type StreamReader interface {
	Next() (StreamEvent, error)
	Close() error
}

// StreamEvent is one upstream event, already translated to OpenAI shape.
type StreamEvent struct {
	Raw []byte

	// Usage carries token counts when this event reported them.
	Usage *domains.TokenUsage

	Terminal  bool
	UsageOnly bool
}
