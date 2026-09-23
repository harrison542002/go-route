// Package repositories holds the Postgres-backed implementations of the outbound repository ports.
package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type AdminRepo struct {
	q    *gen.Queries
	pool *pgxpool.Pool
}

var (
	_ ports.AdminRepository = (*AdminRepo)(nil)
	_ ports.AdminTx         = adminTx{}
)

func NewAdminRepo(pool *pgxpool.Pool) *AdminRepo {
	return &AdminRepo{q: gen.New(pool), pool: pool}
}

func (a *AdminRepo) Tenants() ports.TenantReader { return tenantRepo{q: a.q} }

func (a *AdminRepo) APIKeys() ports.APIKeyReader { return apiKeyRepo{q: a.q} }

func (a *AdminRepo) Quotas() ports.QuotaReader { return quotaRepo{q: a.q} }

func (a *AdminRepo) InTx(ctx context.Context, fn func(ctx context.Context, tx ports.AdminTx) error) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, newAdminTx(a.q.WithTx(tx))); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

// adminTx hands out the same aggregates as AdminRepo, but bound to the
// transaction's queries and typed as the writable interfaces.
type adminTx struct {
	tenants tenantRepo
	apiKeys apiKeyRepo
	quotas  quotaRepo
	audit   auditRepo
}

func newAdminTx(q *gen.Queries) adminTx {
	return adminTx{
		tenants: tenantRepo{q: q},
		apiKeys: apiKeyRepo{q: q},
		quotas:  quotaRepo{q: q},
		audit:   auditRepo{q: q},
	}
}

func (t adminTx) Tenants() ports.TenantRepository { return t.tenants }

func (t adminTx) APIKeys() ports.APIKeyRepository { return t.apiKeys }

func (t adminTx) Quotas() ports.QuotaRepository { return t.quotas }

func (t adminTx) Audit() ports.AuditRepository { return t.audit }

func found(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ports.ErrNotFound
	default:
		return fmt.Errorf("postgres: %s: %w", what, err)
	}
}

type tenantRepo struct {
	q *gen.Queries
}

var _ ports.TenantRepository = tenantRepo{}

func (r tenantRepo) Get(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error) {
	row, err := r.q.GetTenant(ctx, id)
	return tenantAccount(row), found(err, "get tenant")
}

func (r tenantRepo) GetByExternalID(ctx context.Context, externalID string) (domains.TenantAccount, error) {
	row, err := r.q.GetTenantByExternalID(ctx, externalID)
	return tenantAccount(row), found(err, "get tenant")
}

func (r tenantRepo) List(ctx context.Context, f ports.TenantFilter) ([]domains.TenantAccount, error) {
	rows, err := r.q.ListTenants(ctx, gen.ListTenantsParams{
		IncludeDisabled: f.IncludeDisabled,
		RowLimit:        int32(f.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list tenants: %w", err)
	}
	out := make([]domains.TenantAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantAccount(r))
	}
	return out, nil
}

func (r tenantRepo) Lock(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error) {
	row, err := r.q.LockTenant(ctx, id)
	return tenantAccount(row), found(err, "lock tenant")
}

func (r tenantRepo) Create(ctx context.Context, t domains.TenantAccount) (domains.TenantAccount, bool, error) {
	row, err := r.q.CreateTenant(ctx, gen.CreateTenantParams{
		ID:         t.ID,
		ExternalID: string(t.ExternalID),
		Name:       t.Name,
		Metadata:   t.Metadata,
	})
	if err == nil {
		return tenantAccount(row), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domains.TenantAccount{}, false, fmt.Errorf("postgres: create tenant: %w", err)
	}
	existing, err := r.q.GetTenantByExternalID(ctx, string(t.ExternalID))
	if err != nil {
		return domains.TenantAccount{}, false, fmt.Errorf("postgres: read existing tenant: %w", err)
	}
	return tenantAccount(existing), false, nil
}

func (r tenantRepo) Update(ctx context.Context, id uuid.UUID, p ports.TenantPatch) (domains.TenantAccount, error) {
	row, err := r.q.UpdateTenant(ctx, gen.UpdateTenantParams{ID: id, Name: p.Name, Metadata: p.Metadata})
	return tenantAccount(row), found(err, "update tenant")
}

func (r tenantRepo) Disable(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error) {
	row, err := r.q.DisableTenant(ctx, id)
	return tenantAccount(row), found(err, "disable tenant")
}

func (r tenantRepo) Enable(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error) {
	row, err := r.q.EnableTenant(ctx, id)
	return tenantAccount(row), found(err, "enable tenant")
}

type apiKeyRepo struct {
	q *gen.Queries
}

var _ ports.APIKeyRepository = apiKeyRepo{}

func (r apiKeyRepo) Get(ctx context.Context, id uuid.UUID) (domains.APIKey, error) {
	row, err := r.q.GetAPIKey(ctx, id)
	return apiKey(row), found(err, "get api key")
}

func (r apiKeyRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domains.APIKey, error) {
	rows, err := r.q.ListAPIKeysByTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys: %w", err)
	}
	out := make([]domains.APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, apiKey(row))
	}
	return out, nil
}

func (r apiKeyRepo) Create(ctx context.Context, k domains.APIKey, hash []byte) (domains.APIKey, error) {
	row, err := r.q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
		ID:             k.ID,
		TenantID:       k.TenantID,
		KeyHash:        hash,
		KeyPrefix:      k.Prefix,
		ModelAllowlist: k.ModelAllowlist,
	})
	if err != nil {
		return domains.APIKey{}, fmt.Errorf("postgres: create api key: %w", err)
	}
	return apiKey(row), nil
}

func (r apiKeyRepo) UpdateAllowlist(ctx context.Context, id uuid.UUID, allowlist []string) (domains.APIKey, error) {
	row, err := r.q.UpdateAPIKeyAllowlist(ctx, gen.UpdateAPIKeyAllowlistParams{ID: id, ModelAllowlist: allowlist})
	return apiKey(row), found(err, "update api key")
}

func (r apiKeyRepo) Revoke(ctx context.Context, id uuid.UUID) (domains.APIKey, error) {
	row, err := r.q.RevokeAPIKey(ctx, id)
	return apiKey(row), found(err, "revoke api key")
}

type quotaRepo struct {
	q *gen.Queries
}

var _ ports.QuotaRepository = quotaRepo{}

func (r quotaRepo) Get(ctx context.Context, tenantID uuid.UUID, kind domains.WindowKind) (domains.Quota, error) {
	row, err := r.q.GetQuota(ctx, gen.GetQuotaParams{TenantID: tenantID, WindowKind: gen.WindowKind(kind)})
	return quota(row), found(err, "get quota")
}

func (r quotaRepo) List(ctx context.Context, tenantID uuid.UUID) ([]domains.Quota, error) {
	rows, err := r.q.ListQuotas(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list quotas: %w", err)
	}
	out := make([]domains.Quota, 0, len(rows))
	for _, row := range rows {
		out = append(out, quota(row))
	}
	return out, nil
}

func (r quotaRepo) Upsert(ctx context.Context, tenantID uuid.UUID, q domains.Quota) (domains.Quota, error) {
	var maxCost *int64
	if q.MaxCost != nil {
		n := int64(*q.MaxCost)
		maxCost = &n
	}
	row, err := r.q.UpsertQuota(ctx, gen.UpsertQuotaParams{
		TenantID:     tenantID,
		WindowKind:   gen.WindowKind(q.WindowKind),
		PeriodStart:  q.PeriodStart,
		PeriodEnd:    q.PeriodEnd,
		MaxRequests:  q.MaxRequests,
		MaxTokens:    q.MaxTokens,
		MaxCostNanos: maxCost,
		OnExceed:     gen.QuotaAction(q.OnExceed),
	})
	if err != nil {
		return domains.Quota{}, fmt.Errorf("postgres: upsert %s quota: %w", q.WindowKind, err)
	}
	return quota(row), nil
}

func (r quotaRepo) Delete(ctx context.Context, tenantID uuid.UUID, kind domains.WindowKind) (bool, error) {
	n, err := r.q.DeleteQuota(ctx, gen.DeleteQuotaParams{TenantID: tenantID, WindowKind: gen.WindowKind(kind)})
	if err != nil {
		return false, fmt.Errorf("postgres: delete %s quota: %w", kind, err)
	}
	return n > 0, nil
}

func (r quotaRepo) DeleteAll(ctx context.Context, tenantID uuid.UUID) error {
	if err := r.q.DeleteQuotasForTenant(ctx, tenantID); err != nil {
		return fmt.Errorf("postgres: delete quotas: %w", err)
	}
	return nil
}

type auditRepo struct {
	q *gen.Queries
}

var _ ports.AuditRepository = auditRepo{}

func (r auditRepo) Write(ctx context.Context, e ports.AuditEntry) error {
	return insertAudit(ctx, r.q, e)
}

// insertAudit takes the queries rather than hanging off a repository because the
// credential repository audits through the same table from its own transaction.
func insertAudit(ctx context.Context, q *gen.Queries, e ports.AuditEntry) error {
	var detail *string
	if e.Detail != "" {
		detail = &e.Detail
	}
	_, err := q.CreateAuditEntry(ctx, gen.CreateAuditEntryParams{
		ID:             e.ID,
		Ts:             e.At,
		TenantID:       e.TenantID,
		KeyID:          e.KeyID,
		Actor:          e.Actor,
		Action:         e.Action,
		ReasonDetail:   detail,
		Ladder:         []byte("[]"),
		LadderAttempts: []byte("[]"),
	})
	if err != nil {
		return fmt.Errorf("postgres: audit %s: %w", e.Action, err)
	}
	return nil
}

func tenantAccount(r gen.Tenant) domains.TenantAccount {
	return domains.TenantAccount{
		ID:         r.ID,
		ExternalID: domains.Tenant(r.ExternalID),
		Name:       r.Name,
		Metadata:   r.Metadata,
		CreatedAt:  r.CreatedAt,
		DisabledAt: r.DisabledAt,
	}
}

func apiKey(r gen.ApiKey) domains.APIKey {
	return domains.APIKey{
		ID:             r.ID,
		TenantID:       r.TenantID,
		Prefix:         r.KeyPrefix,
		ModelAllowlist: r.ModelAllowlist,
		CreatedAt:      r.CreatedAt,
		LastUsedAt:     r.LastUsedAt,
		RevokedAt:      r.RevokedAt,
	}
}

func quota(r gen.Quota) domains.Quota {
	q := domains.Quota{
		WindowKind:  domains.WindowKind(r.WindowKind),
		PeriodStart: r.PeriodStart,
		PeriodEnd:   r.PeriodEnd,
		MaxRequests: r.MaxRequests,
		MaxTokens:   r.MaxTokens,
		OnExceed:    domains.QuotaAction(r.OnExceed),
		UpdatedAt:   r.UpdatedAt,
	}
	if r.MaxCostNanos != nil {
		c := domains.USD(*r.MaxCostNanos)
		q.MaxCost = &c
	}
	return q
}
