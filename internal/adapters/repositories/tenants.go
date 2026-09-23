package repositories

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// TenantIDs maps the tenant string the gateway carries to the UUID the
// ledger is keyed by. The mapping never changes for a given string, so
// it is cached for the life of the process.
type TenantIDs struct {
	q *gen.Queries

	mu       sync.RWMutex
	byTenant map[domains.Tenant]uuid.UUID
	byID     map[uuid.UUID]domains.Tenant
}

func NewTenantIDs(q *gen.Queries) *TenantIDs {
	return &TenantIDs{
		byTenant: make(map[domains.Tenant]uuid.UUID),
		byID:     make(map[uuid.UUID]domains.Tenant),
		q:        q,
	}
}

func (t *TenantIDs) Lookup(ctx context.Context, tenant domains.Tenant) (uuid.UUID, error) {
	if id, ok := t.get(tenant); ok {
		return id, nil
	}

	row, err := t.q.GetTenantByExternalID(ctx, string(tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: %s", ports.ErrUnknownTenant, tenant)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("postgres: lookup tenant %s: %w", tenant, err)
	}

	t.put(tenant, row.ID)
	return row.ID, nil
}

// Resolve maps a UUID or external id to the external id the ledger and audit
// log are read by. A value that parses as a UUID is tried as ours first and
// then as an external id, matching how every other admin path reads {tenant}.
func (t *TenantIDs) Resolve(ctx context.Context, ref string) (domains.Tenant, error) {
	if id, err := uuid.Parse(ref); err == nil {
		name, err := t.Name(ctx, id)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, ports.ErrUnknownTenant) {
			return "", err
		}
	}

	if _, err := t.Lookup(ctx, domains.Tenant(ref)); err != nil {
		return "", err
	}
	return domains.Tenant(ref), nil
}

func (t *TenantIDs) Name(ctx context.Context, id uuid.UUID) (domains.Tenant, error) {
	t.mu.RLock()
	name, ok := t.byID[id]
	t.mu.RUnlock()
	if ok {
		return name, nil
	}

	row, err := t.q.GetTenant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ports.ErrUnknownTenant, id)
	}
	if err != nil {
		return "", fmt.Errorf("postgres: lookup tenant %s: %w", id, err)
	}

	t.put(domains.Tenant(row.ExternalID), row.ID)
	return domains.Tenant(row.ExternalID), nil
}

func (t *TenantIDs) get(tenant domains.Tenant) (uuid.UUID, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.byTenant[tenant]
	return id, ok
}

func (t *TenantIDs) put(tenant domains.Tenant, id uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byTenant[tenant] = id
	t.byID[id] = tenant
}
