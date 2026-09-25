package domains

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TenantAccount is the whole tenant row; the gateway only ever carries ExternalID.
type TenantAccount struct {
	ID uuid.UUID

	// ExternalID is the tenant's id in the customer's own system, and the
	// string every routed request is attributed to.
	ExternalID Tenant
	Name       string

	// Metadata is opaque to go-route: stored, never read.
	Metadata []byte

	CreatedAt  time.Time
	DisabledAt *time.Time
}

func (t TenantAccount) Disabled() bool { return t.DisabledAt != nil }

// APIKey is a credential as the admin API shows it. The secret is never stored:
// only its SHA-256, and the plaintext exists once, in the response that created it.
type APIKey struct {
	ID       uuid.UUID
	TenantID uuid.UUID

	// Prefix identifies a key in a UI. Identification only, never authentication.
	Prefix string

	// ModelAllowlist is the set of aliases this key may request. Nil
	// allows every alias; empty allows none.
	ModelAllowlist []string

	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// Role is what an admin credential may do. Deliberately two values rather than
// per-resource scopes: the only line anyone has asked for is between reading and changing.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleReadonly Role = "readonly"
)

// Roles lists every role in the order the database enum declares them.
var Roles = []Role{RoleAdmin, RoleReadonly}

func (r Role) Valid() bool {
	for _, known := range Roles {
		if r == known {
			return true
		}
	}
	return false
}

// Permission is what a request asks of the credential making it.
type Permission string

const (
	PermissionRead  Permission = "read"
	PermissionWrite Permission = "write"
)

// Allows reports whether the role permits p. An unrecognised role -- a row
// written by a newer build against the same database -- permits nothing.
func (r Role) Allows(p Permission) bool {
	switch r {
	case RoleAdmin:
		return true
	case RoleReadonly:
		return p == PermissionRead
	default:
		return false
	}
}

// AdminCredential is one caller of the internal admin API. It belongs to no
// tenant. The secret is never stored: only its SHA-256, and the plaintext
// exists once, when the CLI prints it.
type AdminCredential struct {
	ID uuid.UUID

	// Name is what the audit log records. It never changes, so old rows keep
	// meaning what they meant.
	Name string

	// Prefix identifies a credential in a listing. Identification only, never
	// authentication.
	Prefix string

	Role Role

	// ExpiresAt is nil for a credential that does not expire. Expiry is checked
	// when the token is presented rather than swept, so an expired credential
	// stays as attributable in the audit log as a revoked one.
	ExpiresAt *time.Time

	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (c AdminCredential) Revoked() bool { return c.RevokedAt != nil }

// Expired reports whether the credential's lifetime has run out. The boundary
// is inclusive: a credential that expires at noon is dead at noon.
func (c AdminCredential) Expired(now time.Time) bool {
	return c.ExpiresAt != nil && !now.Before(*c.ExpiresAt)
}

// Actor is the string written to audit_log.actor for every mutation this
// credential makes.
func (c AdminCredential) Actor() string { return "admin:" + c.Name }

// WindowKind is how a quota window is bounded.
type WindowKind string

const (
	WindowMinute WindowKind = "minute"
	WindowHour   WindowKind = "hour"
	WindowDay    WindowKind = "day"
	WindowMonth  WindowKind = "month"

	// WindowPeriod carries explicit start and end dates, which is what makes
	// anniversary billing work for customers who signed up mid-month.
	WindowPeriod WindowKind = "period"
)

// WindowKinds lists every kind in the order the database enum declares them,
// which is the order quota sets are returned in.
var WindowKinds = []WindowKind{WindowMinute, WindowHour, WindowDay, WindowMonth, WindowPeriod}

func (k WindowKind) Valid() bool {
	for _, known := range WindowKinds {
		if k == known {
			return true
		}
	}
	return false
}

// Rank orders kinds as the database enum does.
func (k WindowKind) Rank() int {
	for i, known := range WindowKinds {
		if k == known {
			return i
		}
	}
	return len(WindowKinds)
}

// QuotaAction is what happens when a quota is reached.
type QuotaAction string

const (
	// QuotaBlock rejects the request with a 429.
	QuotaBlock QuotaAction = "block"

	// QuotaAllow records the overage and keeps serving.
	QuotaAllow QuotaAction = "allow"
)

// Quota is one spend cap on one window for one tenant.
type Quota struct {
	WindowKind WindowKind

	// PeriodStart and PeriodEnd are set exactly when WindowKind is WindowPeriod.
	PeriodStart *time.Time
	PeriodEnd   *time.Time

	// MaxCost is what may be spent in the window. Zero is a real cap that
	// allows nothing, not an absent one.
	MaxCost USD

	OnExceed  QuotaAction
	UpdatedAt time.Time
}

var ErrInvalidQuota = errors.New("quota: invalid")

// Validate mirrors the quotas table's CHECK constraints, so a caller gets a
// sentence about what it sent rather than a constraint name.
func (q Quota) Validate() error {
	if !q.WindowKind.Valid() {
		return fmt.Errorf("%w: unknown window_kind %q (want minute, hour, day, month or period)",
			ErrInvalidQuota, q.WindowKind)
	}

	hasStart, hasEnd := q.PeriodStart != nil, q.PeriodEnd != nil
	if q.WindowKind == WindowPeriod {
		if !hasStart || !hasEnd {
			return fmt.Errorf("%w: a period quota needs both period_start and period_end", ErrInvalidQuota)
		}
		if !q.PeriodStart.Before(*q.PeriodEnd) {
			return fmt.Errorf("%w: period_start must be before period_end", ErrInvalidQuota)
		}
	} else if hasStart || hasEnd {
		return fmt.Errorf("%w: period_start and period_end are only valid for window_kind period, not %q",
			ErrInvalidQuota, q.WindowKind)
	}

	if q.MaxCost < 0 {
		return fmt.Errorf("%w: %s quota max_cost_nanos must not be negative", ErrInvalidQuota, q.WindowKind)
	}

	switch q.OnExceed {
	case QuotaBlock, QuotaAllow:
	default:
		return fmt.Errorf("%w: unknown on_exceed %q (want block or allow)", ErrInvalidQuota, q.OnExceed)
	}
	return nil
}

// SameLimits reports whether two quotas describe the same limit, ignoring when
// either was written, so a sync job can reassert desired state without rewriting.
func (q Quota) SameLimits(o Quota) bool {
	return q.WindowKind == o.WindowKind &&
		q.OnExceed == o.OnExceed &&
		sameTime(q.PeriodStart, o.PeriodStart) &&
		sameTime(q.PeriodEnd, o.PeriodEnd) &&
		q.MaxCost == o.MaxCost
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
