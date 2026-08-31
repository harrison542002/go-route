package routing

import (
	"fmt"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// Resolver turns target descriptions into dialable targets. It is the
// seam between the domain (which reasons about names) and dispatch
// (which needs live clients).
type Resolver struct {
	providers map[string]ports.Provider
}

func NewResolver(providers map[string]ports.Provider) *Resolver {
	return &Resolver{providers: providers}
}

// Resolve maps a ladder onto dialable targets.
//
// A missing provider is a programming error, not a runtime condition:
// config validation proves every reference resolves before boot.
func (r *Resolver) Resolve(l domains.Ladder) ([]ports.Target, error) {
	out := make([]ports.Target, 0, len(l.Targets))
	for _, ref := range l.Targets {
		p, ok := r.providers[ref.Provider]
		if !ok {
			return nil, fmt.Errorf("routing: no provider %q for target %q", ref.Provider, ref.Name)
		}
		out = append(out, ports.Target{
			Provider: p,
			Model:    ref.UpstreamModel,
			Ref:      ref, // carried through so the decision record can name it
		})
	}
	return out, nil
}
