package routing

import (
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// FromConfig builds the routing table and resolver from validated config.
func FromConfig(cfg *config.Config, providers map[string]ports.Provider) (*Table, *Resolver, error) {
	refs := make(map[string]domains.TargetRef, len(cfg.Targets))
	for name, tc := range cfg.Targets {
		refs[name] = domains.TargetRef{
			Name:          name,
			Provider:      tc.Provider,
			UpstreamModel: tc.Model,
			Region:        tc.Region,
		}
	}

	ladders := make(map[string]domains.Ladder, len(cfg.Models))
	for alias, names := range cfg.Models {
		targets := make([]domains.TargetRef, 0, len(names))
		for _, n := range names {
			targets = append(targets, refs[n]) // validated at load
		}
		ladders[alias] = domains.Ladder{
			Targets: targets,
			Reason:  domains.Reason{Kind: domains.ReasonModelAlias, ModelAlias: alias},
		}
	}

	table, err := NewTable(ladders)
	if err != nil {
		return nil, nil, err
	}
	return table, NewResolver(providers), nil
}
