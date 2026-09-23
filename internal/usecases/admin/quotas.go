package admin

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/oapi-codegen/nullable"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

func (s *Service) ListQuotas(ctx context.Context, tenantRef string) ([]domains.Quota, error) {
	t, err := resolveTenant(ctx, s.repo.Tenants(), tenantRef)
	if err != nil {
		return nil, err
	}
	return s.repo.Quotas().List(ctx, t.ID)
}

func (s *Service) GetQuota(ctx context.Context, tenantRef string, kind domains.WindowKind) (domains.Quota, error) {
	if !kind.Valid() {
		return domains.Quota{}, invalid("unknown window_kind %q", kind)
	}
	t, err := resolveTenant(ctx, s.repo.Tenants(), tenantRef)
	if err != nil {
		return domains.Quota{}, err
	}
	q, err := s.repo.Quotas().Get(ctx, t.ID, kind)
	if errors.Is(err, ports.ErrNotFound) {
		return domains.Quota{}, fmt.Errorf("%w: tenant %q has no %s quota", ports.ErrNotFound, tenantRef, kind)
	}
	return q, err
}

// ReplaceQuotas makes set the tenant's whole quota set, in one transaction:
// kinds left out are removed, and a failure part way must never leave the old
// set deleted and the new one unwritten, which would read as unlimited. An
// empty set removes every limit.
func (s *Service) ReplaceQuotas(
	ctx context.Context, w ports.Write, tenantRef string, set []domains.Quota,
) ([]domains.Quota, error) {
	set, err := validQuotaSet(set)
	if err != nil {
		return nil, err
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) ([]domains.Quota, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil {
			return nil, err
		}

		current, err := tx.Quotas().List(ctx, t.ID)
		if err != nil {
			return nil, err
		}

		if sameQuotaSet(current, set) {
			return current, nil
		}

		if err := tx.Quotas().DeleteAll(ctx, t.ID); err != nil {
			return nil, err
		}
		written := make([]domains.Quota, 0, len(set))
		for _, q := range set {
			stored, err := tx.Quotas().Upsert(ctx, t.ID, q)
			if err != nil {
				return nil, err
			}
			written = append(written, stored)
		}

		if err := s.audit(ctx, tx, w, "quota.replace", &t.ID, nil, quotaDetails(written)); err != nil {
			return nil, err
		}
		return written, nil
	})
}

// PutQuota creates or replaces the one quota for q.WindowKind, leaving the
// tenant's other windows alone.
func (s *Service) PutQuota(ctx context.Context, w ports.Write, tenantRef string, q domains.Quota) (domains.Quota, error) {
	q, err := validQuota(q)
	if err != nil {
		return domains.Quota{}, err
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.Quota, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil {
			return domains.Quota{}, err
		}

		current, err := tx.Quotas().Get(ctx, t.ID, q.WindowKind)
		switch {
		case err == nil && current.SameLimits(q):
			return current, nil
		case err != nil && !errors.Is(err, ports.ErrNotFound):
			return domains.Quota{}, err
		}

		stored, err := tx.Quotas().Upsert(ctx, t.ID, q)
		if err != nil {
			return domains.Quota{}, err
		}
		if err := s.audit(ctx, tx, w, "quota.upsert", &t.ID, nil, quotaDetails([]domains.Quota{stored})); err != nil {
			return domains.Quota{}, err
		}
		return stored, nil
	})
}

// DeleteQuota removes one window's limit; deleting one that is not there
// succeeds.
func (s *Service) DeleteQuota(ctx context.Context, w ports.Write, tenantRef string, kind domains.WindowKind) error {
	if !kind.Valid() {
		return invalid("unknown window_kind %q", kind)
	}

	_, err := mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (struct{}, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil {
			return struct{}{}, err
		}

		deleted, err := tx.Quotas().Delete(ctx, t.ID, kind)
		if err != nil || !deleted {
			return struct{}{}, err
		}
		detail := map[string]string{"window_kind": string(kind)}
		return struct{}{}, s.audit(ctx, tx, w, "quota.delete", &t.ID, nil, detail)
	})
	return err
}

// validQuota applies defaults and checks q, so the caller hears what was wrong
// with its input rather than the name of a CHECK constraint.
func validQuota(q domains.Quota) (domains.Quota, error) {
	if q.OnExceed == "" {
		q.OnExceed = domains.QuotaBlock
	}
	if err := q.Validate(); err != nil {
		return domains.Quota{}, fmt.Errorf("%w: %w", ports.ErrInvalid, err)
	}
	return q, nil
}

func validQuotaSet(set []domains.Quota) ([]domains.Quota, error) {
	out := make([]domains.Quota, 0, len(set))
	seen := map[domains.WindowKind]bool{}

	for _, q := range set {
		q, err := validQuota(q)
		if err != nil {
			return nil, err
		}
		if seen[q.WindowKind] {
			// One row per kind, so picking between duplicates would be a guess.
			return nil, invalid("window_kind %q appears more than once", q.WindowKind)
		}
		seen[q.WindowKind] = true
		out = append(out, q)
	}

	// Written and returned in the enum's order, matching what a later read
	// returns.
	slices.SortFunc(out, func(a, b domains.Quota) int { return a.WindowKind.Rank() - b.WindowKind.Rank() })
	return out, nil
}

func sameQuotaSet(current, desired []domains.Quota) bool {
	if len(current) != len(desired) {
		return false
	}
	byKind := make(map[domains.WindowKind]domains.Quota, len(current))
	for _, q := range current {
		byKind[q.WindowKind] = q
	}
	for _, q := range desired {
		c, ok := byKind[q.WindowKind]
		if !ok || !c.SameLimits(q) {
			return false
		}
	}
	return true
}

// quotaDetails renders the whole set a quota mutation wrote, for audit_log. A
// nil limit becomes an explicit null, not the zero an unspecified nullable
// marshals as: unlimited and 0 are opposite facts.
func quotaDetails(set []domains.Quota) []gen.QuotaAuditDetail {
	out := make([]gen.QuotaAuditDetail, 0, len(set))
	for _, q := range set {
		d := gen.QuotaAuditDetail{
			WindowKind:   gen.WindowKind(q.WindowKind),
			PeriodStart:  q.PeriodStart,
			PeriodEnd:    q.PeriodEnd,
			MaxRequests:  nullableOf(q.MaxRequests),
			MaxTokens:    nullableOf(q.MaxTokens),
			MaxCostNanos: nullable.NewNullNullable[int64](),
			OnExceed:     gen.QuotaAction(q.OnExceed),
		}
		if q.MaxCost != nil {
			d.MaxCostNanos = nullable.NewNullableWithValue(int64(*q.MaxCost))
		}
		out = append(out, d)
	}
	return out
}

// nullableOf renders a nil pointer as an explicit JSON null, where a
// nullable.Nullable left unspecified would marshal as T's zero value.
func nullableOf[T any](p *T) nullable.Nullable[T] {
	if p == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*p)
}
