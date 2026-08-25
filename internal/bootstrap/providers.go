package bootstrap

import (
	"fmt"

	"github.com/harrison542002/go-route/internal/adapters/outbound/providers/oaicompat"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
)

func buildProviders(cfg *config.Config) (map[string]ports.Provider, error) {
	providers := make(map[string]ports.Provider, len(cfg.Providers))

	for name, pc := range cfg.Providers {
		switch pc.Type {
		case "oaicompat":
			p, err := oaicompat.New(oaicompat.Config{
				Name:                name,
				BaseURL:             pc.BaseURL,
				APIKey:              pc.APIKey,
				DisableStreamOption: pc.DisableStreamOptions,
				ExtraHeaders:        pc.ExtraHeaders,
			})
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			providers[name] = p

		default:
			// Unreachable: config.validate rejects unknown types at load.
			// Kept so adding an adapter without wiring it here fails loudly
			// rather than silently producing a nil provider.
			return nil, fmt.Errorf("provider %q: unsupported type %q", name, pc.Type)
		}
	}

	return providers, nil
}
