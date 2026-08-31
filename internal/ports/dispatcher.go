package ports

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
)

type Target struct {
	Provider Provider
	Model    string
	Ref      domains.TargetRef
}

func (t Target) String() string {
	if t.Ref.Name == "" {
		return "unnamed:" + t.Model
	}
	return t.Ref.Name
}

type Dispatcher interface {
	Run(ctx context.Context, ladder []Target, req *ProviderRequest, out ClientStream) domains.Outcome
}
