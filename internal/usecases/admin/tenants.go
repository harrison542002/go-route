package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

const maxNameLen = 256

func (s *Service) CreateTenant(ctx context.Context, w ports.Write, in ports.NewTenant) (ports.TenantCreated, error) {
	if err := validExternalID(in.ExternalID); err != nil {
		return ports.TenantCreated{}, err
	}
	if err := validName(in.Name); err != nil {
		return ports.TenantCreated{}, err
	}
	metadata, err := metadataObject(in.Metadata)
	if err != nil {
		return ports.TenantCreated{}, err
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (ports.TenantCreated, error) {
		id, err := newID()
		if err != nil {
			return ports.TenantCreated{}, err
		}

		t, created, err := tx.Tenants().Create(ctx, domains.TenantAccount{
			ID:         id,
			ExternalID: domains.Tenant(in.ExternalID),
			Name:       in.Name,
			Metadata:   metadata,
		})
		if err != nil {
			return ports.TenantCreated{}, err
		}

		if !created {
			// external_id is unique so a retrying signup cannot make two tenants:
			// the retry gets the original back, and only a different tenant
			// claiming the same id is a conflict.
			if t.Name != in.Name || !sameJSON(t.Metadata, metadata) {
				return ports.TenantCreated{}, fmt.Errorf(
					"%w: tenant %q already exists with a different name or metadata",
					ports.ErrConflict, in.ExternalID)
			}
			return ports.TenantCreated{Tenant: t}, nil
		}

		if err := s.audit(ctx, tx, w, "tenant.create", &t.ID, nil, tenantDetail(t)); err != nil {
			return ports.TenantCreated{}, err
		}
		return ports.TenantCreated{Tenant: t, Created: true}, nil
	})
}

func (s *Service) ListTenants(ctx context.Context, f ports.TenantFilter) ([]domains.TenantAccount, error) {
	switch {
	case f.Limit == 0:
		f.Limit = ports.DefaultTenantLimit
	case f.Limit < 0 || f.Limit > ports.MaxTenantLimit:
		return nil, invalid("limit must be between 1 and %d", ports.MaxTenantLimit)
	}
	return s.repo.Tenants().List(ctx, f)
}

func (s *Service) GetTenant(ctx context.Context, tenantRef string) (domains.TenantAccount, error) {
	return resolveTenant(ctx, s.repo.Tenants(), tenantRef)
}

func (s *Service) UpdateTenant(
	ctx context.Context, w ports.Write, tenantRef string, p ports.TenantPatch,
) (domains.TenantAccount, error) {
	if p.Name == nil && p.Metadata == nil {
		return domains.TenantAccount{}, invalid("nothing to update: set name, metadata or both")
	}
	if p.Name != nil {
		if err := validName(*p.Name); err != nil {
			return domains.TenantAccount{}, err
		}
	}
	if p.Metadata != nil {
		metadata, err := metadataObject(p.Metadata)
		if err != nil {
			return domains.TenantAccount{}, err
		}
		p.Metadata = metadata
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.TenantAccount, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil {
			return domains.TenantAccount{}, err
		}

		nameSame := p.Name == nil || *p.Name == t.Name
		metaSame := p.Metadata == nil || sameJSON(p.Metadata, t.Metadata)
		if nameSame && metaSame {
			return t, nil
		}

		t, err = tx.Tenants().Update(ctx, t.ID, p)
		if err != nil {
			return domains.TenantAccount{}, err
		}
		if err := s.audit(ctx, tx, w, "tenant.update", &t.ID, nil, tenantDetail(t)); err != nil {
			return domains.TenantAccount{}, err
		}
		return t, nil
	})
}

// DisableTenant is the admin API's delete: tenants are never removed, because
// ledger rows refer to them.
func (s *Service) DisableTenant(ctx context.Context, w ports.Write, tenantRef string) (domains.TenantAccount, error) {
	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.TenantAccount, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil || t.Disabled() {
			return t, err
		}

		t, err = tx.Tenants().Disable(ctx, t.ID)
		if err != nil {
			return domains.TenantAccount{}, err
		}
		if err := s.audit(ctx, tx, w, "tenant.disable", &t.ID, nil, tenantDetail(t)); err != nil {
			return domains.TenantAccount{}, err
		}
		return t, nil
	})
}

func (s *Service) EnableTenant(ctx context.Context, w ports.Write, tenantRef string) (domains.TenantAccount, error) {
	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.TenantAccount, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil || !t.Disabled() {
			return t, err
		}

		t, err = tx.Tenants().Enable(ctx, t.ID)
		if err != nil {
			return domains.TenantAccount{}, err
		}
		if err := s.audit(ctx, tx, w, "tenant.enable", &t.ID, nil, tenantDetail(t)); err != nil {
			return domains.TenantAccount{}, err
		}
		return t, nil
	})
}

// resolveTenant accepts the tenant's UUID or its external id. A ref that parses
// as a UUID is tried as ours first; an external id that happens to look like a
// UUID still resolves, through the fallback.
func resolveTenant(ctx context.Context, r ports.TenantReader, ref string) (domains.TenantAccount, error) {
	if ref == "" {
		return domains.TenantAccount{}, invalid("tenant is required")
	}

	if id, err := uuid.Parse(ref); err == nil {
		t, err := r.Get(ctx, id)
		if !errors.Is(err, ports.ErrNotFound) {
			return t, err
		}
	}

	t, err := r.GetByExternalID(ctx, ref)
	if errors.Is(err, ports.ErrNotFound) {
		return domains.TenantAccount{}, fmt.Errorf("%w: tenant %q", ports.ErrNotFound, ref)
	}
	return t, err
}

func lockTenant(ctx context.Context, tr ports.TenantRepository, ref string) (domains.TenantAccount, error) {
	t, err := resolveTenant(ctx, tr, ref)
	if err != nil {
		return domains.TenantAccount{}, err
	}
	return tr.Lock(ctx, t.ID)
}

func validExternalID(id string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return invalid("external_id is required")
	case id != strings.TrimSpace(id):
		// Rejected rather than trimmed: the id has to match the customer's own
		// system byte for byte, or the join they reconcile with breaks.
		return invalid("external_id must not start or end with whitespace")
	case len(id) > domains.MaxTenantLen:
		return invalid("external_id must be at most %d bytes", domains.MaxTenantLen)
	case !utf8.ValidString(id):
		return invalid("external_id must be valid UTF-8")
	}
	return nil
}

func validName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return invalid("name is required")
	case utf8.RuneCountInString(name) > maxNameLen:
		return invalid("name must be at most %d characters", maxNameLen)
	}
	return nil
}

// metadataObject insists on a JSON object, which is what the column is
// documented to hold.
func metadataObject(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalid("metadata must be a JSON object")
	}
	return raw, nil
}

// sameJSON compares by meaning rather than bytes: Postgres re-serialises jsonb,
// reordering keys and rewriting numbers, so what is read back is never
// byte-identical to what was sent.
func sameJSON(a, b []byte) bool {
	x, err := decodeJSON(a)
	if err != nil {
		return false
	}
	y, err := decodeJSON(b)
	if err != nil {
		return false
	}
	return equalJSON(x, y)
}

// decodeJSON keeps numbers as their literal text: a float64 round-trip would
// lose large integers, and comparing them as exact rationals is what makes 1e3
// equal the 1000 Postgres hands back.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func equalJSON(x, y any) bool {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for k, v := range xv {
			w, ok := yv[k]
			if !ok || !equalJSON(v, w) {
				return false
			}
		}
		return true

	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for i := range xv {
			if !equalJSON(xv[i], yv[i]) {
				return false
			}
		}
		return true

	case json.Number:
		yv, ok := y.(json.Number)
		if !ok {
			return false
		}
		a, okA := new(big.Rat).SetString(xv.String())
		b, okB := new(big.Rat).SetString(yv.String())
		return okA && okB && a.Cmp(b) == 0

	default:
		return x == y
	}
}

// tenantDetail renders the state a tenant mutation leaves behind, for
// audit_log. Metadata is never empty on the way out: an empty json.RawMessage
// is not JSON at all.
func tenantDetail(t domains.TenantAccount) gen.TenantAuditDetail {
	meta := json.RawMessage(t.Metadata)
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	return gen.TenantAuditDetail{
		ExternalId: string(t.ExternalID),
		Name:       t.Name,
		Metadata:   meta,
		Disabled:   t.Disabled(),
	}
}
