package sink

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
)

type Writer interface {
	Write(ctx context.Context, batch []domains.RoutingDecision) error
}
