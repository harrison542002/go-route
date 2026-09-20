package postgresql

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// Auth resolves API keys against the api_keys table. It reads through a
// pool it does not own; closing that is the composition root's business.
type Auth struct {
	q *gen.Queries
}

var _ ports.Authenticator = (*Auth)(nil)

func NewAuth(pool *pgxpool.Pool) *Auth {
	return &Auth{q: gen.New(pool)}
}

func (a *Auth) Authenticate(ctx context.Context, presented string) (ports.Identity, error) {
	if presented == "" {
		return ports.Identity{}, ports.ErrNoCredentials
	}

	sum := sha256.Sum256([]byte(presented))

	row, err := a.q.GetAPIKeyByHash(ctx, sum[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.Identity{}, ports.ErrUnknownKey
	}
	if err != nil {
		return ports.Identity{}, fmt.Errorf("postgres: authenticate: %w", err)
	}

	if row.ApiKey.RevokedAt != nil {
		return ports.Identity{}, ports.ErrKeyRevoked
	}
	if row.Tenant.DisabledAt != nil {
		return ports.Identity{}, fmt.Errorf("%w: %s", ports.ErrTenantDisabled, row.Tenant.ExternalID)
	}

	return ports.Identity{
		Tenant:    domains.Tenant(row.Tenant.ExternalID),
		KeyID:     row.ApiKey.ID,
		Allowlist: row.ApiKey.ModelAllowlist,
	}, nil
}
