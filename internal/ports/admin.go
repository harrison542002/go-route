//go:generate mockgen -source=admin.go -destination=mocks/admin_mock.go -package=mocks

package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

const (
	DefaultTenantLimit = 100
	MaxTenantLimit     = 1000
)

var (
	ErrNotFound = errors.New("admin: not found")
	ErrConflict = errors.New("admin: conflict")
	ErrInvalid  = errors.New("admin: invalid request")
	// ErrUnauthenticated is every way a login or a refresh can fail. It
	// carries no detail: the response to a wrong password, an unknown
	// email, a disabled person and a dead refresh token is one 401.
	ErrUnauthenticated = errors.New("admin: unauthenticated")
)

// Write carries who is making an admin mutation.
type Write struct {
	// Actor is recorded on the audit row, as admin:<subject>.
	Actor string
}

type NewTenant struct {
	ExternalID string
	Name       string
	Metadata   []byte
}

// TenantPatch changes only the fields that are set. ExternalID is deliberately
// absent: it is the join key into the customer's own system.
type TenantPatch struct {
	Name     *string
	Metadata []byte
}

// TenantCreated says whether the tenant was made by this call or already
// existed with the same name and metadata, so a retried signup gets its
// original tenant back rather than a 409.
type TenantCreated struct {
	Tenant  domains.TenantAccount
	Created bool
}

type TenantFilter struct {
	IncludeDisabled bool
	Limit           int
}

type NewAPIKey struct {
	// ModelAllowlist is nil to allow every alias, empty to allow none.
	ModelAllowlist []string
}

// IssuedAPIKey is the only place the plaintext of a key ever exists.
type IssuedAPIKey struct {
	Key    domains.APIKey
	Secret string
}

// APIKeyPatch replaces the allowlist. Nil allows every alias.
type APIKeyPatch struct {
	ModelAllowlist []string
}

// Admin is what the admin API offers. Every method taking a Write is a
// mutation and is audited; tenantRef accepts a tenant UUID or external id.
type Admin interface {
	CreateTenant(ctx context.Context, w Write, in NewTenant) (TenantCreated, error)
	ListTenants(ctx context.Context, f TenantFilter) ([]domains.TenantAccount, error)
	GetTenant(ctx context.Context, tenantRef string) (domains.TenantAccount, error)
	UpdateTenant(ctx context.Context, w Write, tenantRef string, p TenantPatch) (domains.TenantAccount, error)
	DisableTenant(ctx context.Context, w Write, tenantRef string) (domains.TenantAccount, error)
	EnableTenant(ctx context.Context, w Write, tenantRef string) (domains.TenantAccount, error)

	CreateAPIKey(ctx context.Context, w Write, tenantRef string, in NewAPIKey) (IssuedAPIKey, error)
	ListAPIKeys(ctx context.Context, tenantRef string) ([]domains.APIKey, error)
	GetAPIKey(ctx context.Context, id uuid.UUID) (domains.APIKey, error)
	UpdateAPIKey(ctx context.Context, w Write, id uuid.UUID, p APIKeyPatch) (domains.APIKey, error)
	RevokeAPIKey(ctx context.Context, w Write, id uuid.UUID) (domains.APIKey, error)

	ListQuotas(ctx context.Context, tenantRef string) ([]domains.Quota, error)
	GetQuota(ctx context.Context, tenantRef string, kind domains.WindowKind) (domains.Quota, error)
	ReplaceQuotas(ctx context.Context, w Write, tenantRef string, set []domains.Quota) ([]domains.Quota, error)
	PutQuota(ctx context.Context, w Write, tenantRef string, q domains.Quota) (domains.Quota, error)
	DeleteQuota(ctx context.Context, w Write, tenantRef string, kind domains.WindowKind) error
}

// TenantReader reads tenants. Single-row lookups return ErrNotFound when there
// is none.
type TenantReader interface {
	Get(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error)
	GetByExternalID(ctx context.Context, externalID string) (domains.TenantAccount, error)
	List(ctx context.Context, f TenantFilter) ([]domains.TenantAccount, error)
}

// TenantRepository is the tenant aggregate with its writes, reachable only
// inside a transaction.
type TenantRepository interface {
	TenantReader

	// Lock reads a tenant and holds it until the transaction ends. Every
	// mutation must take it first, so writers for one tenant serialise.
	Lock(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error)

	// Create returns created=false, and the existing row, when the external id
	// is already taken.
	Create(ctx context.Context, t domains.TenantAccount) (tenant domains.TenantAccount, created bool, err error)
	Update(ctx context.Context, id uuid.UUID, p TenantPatch) (domains.TenantAccount, error)
	Disable(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error)
	Enable(ctx context.Context, id uuid.UUID) (domains.TenantAccount, error)
}

// APIKeyReader reads API keys, ErrNotFound when there is no such key.
type APIKeyReader interface {
	Get(ctx context.Context, id uuid.UUID) (domains.APIKey, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domains.APIKey, error)
}

// APIKeyRepository is the API key aggregate with its writes, reachable only
// inside a transaction.
type APIKeyRepository interface {
	APIKeyReader

	Create(ctx context.Context, k domains.APIKey, hash []byte) (domains.APIKey, error)
	UpdateAllowlist(ctx context.Context, id uuid.UUID, allowlist []string) (domains.APIKey, error)
	Revoke(ctx context.Context, id uuid.UUID) (domains.APIKey, error)
}

// QuotaReader reads quotas, ErrNotFound when the tenant has no limit for that
// window.
type QuotaReader interface {
	Get(ctx context.Context, tenantID uuid.UUID, kind domains.WindowKind) (domains.Quota, error)
	List(ctx context.Context, tenantID uuid.UUID) ([]domains.Quota, error)
}

// QuotaRepository is the quota aggregate with its writes, reachable only inside
// a transaction.
type QuotaRepository interface {
	QuotaReader

	Upsert(ctx context.Context, tenantID uuid.UUID, q domains.Quota) (domains.Quota, error)
	Delete(ctx context.Context, tenantID uuid.UUID, kind domains.WindowKind) (deleted bool, err error)
	DeleteAll(ctx context.Context, tenantID uuid.UUID) error
}

// AuditRepository writes one audit row.
type AuditRepository interface {
	Write(ctx context.Context, e AuditEntry) error
}

// AdminRepository is where tenants, keys and quotas live. Outside a transaction
// it hands out readers only, so a mutation is reachable solely through the
// repositories InTx supplies.
type AdminRepository interface {
	Tenants() TenantReader
	APIKeys() APIKeyReader
	Quotas() QuotaReader

	// InTx runs fn in one transaction, committing when it returns nil. fn must
	// use the context it is handed: that is what the transaction runs under.
	InTx(ctx context.Context, fn func(ctx context.Context, tx AdminTx) error) error
}

// AdminTx is the writable side of AdminRepository, reachable only inside a
// transaction; the audit row commits with the change it describes.
type AdminTx interface {
	Tenants() TenantRepository
	APIKeys() APIKeyRepository
	Quotas() QuotaRepository
	Audit() AuditRepository
}

// AuditEntry is one admin mutation, written in the transaction that made it so
// a change that rolls back leaves no row claiming it happened.
type AuditEntry struct {
	ID       uuid.UUID
	At       time.Time
	TenantID *uuid.UUID
	KeyID    *uuid.UUID
	Actor    string
	Action   string
	Detail   string
}
