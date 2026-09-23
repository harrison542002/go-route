package admin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

const (
	// KeyPrefix marks a go-route credential, so a leaked key is recognisable and
	// secret scanners can match it.
	KeyPrefix = "gr_live_"

	maxAllowlistLen = 256
	maxAliasLen     = 256
)

// CreateAPIKey is the one mutation a repeat does not leave alone: every call
// makes a new secret, so a retry leaves a second key to revoke.
func (s *Service) CreateAPIKey(
	ctx context.Context, w ports.Write, tenantRef string, in ports.NewAPIKey,
) (ports.IssuedAPIKey, error) {
	allowlist, err := validAllowlist(in.ModelAllowlist)
	if err != nil {
		return ports.IssuedAPIKey{}, err
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (ports.IssuedAPIKey, error) {
		t, err := lockTenant(ctx, tx.Tenants(), tenantRef)
		if err != nil {
			return ports.IssuedAPIKey{}, err
		}
		if t.Disabled() {
			// The key would be rejected at ingress anyway.
			return ports.IssuedAPIKey{}, fmt.Errorf(
				"%w: tenant %q is disabled; enable it before issuing keys", ports.ErrConflict, t.ExternalID)
		}

		minted, err := Mint(KeyPrefix, s.random)
		if err != nil {
			return ports.IssuedAPIKey{}, err
		}
		id, err := newID()
		if err != nil {
			return ports.IssuedAPIKey{}, err
		}

		key, err := tx.APIKeys().Create(ctx, domains.APIKey{
			ID:             id,
			TenantID:       t.ID,
			Prefix:         minted.Prefix,
			ModelAllowlist: allowlist,
		}, minted.Hash[:])
		if err != nil {
			return ports.IssuedAPIKey{}, err
		}

		if err := s.audit(ctx, tx, w, "key.create", &t.ID, &key.ID, keyDetail(key)); err != nil {
			return ports.IssuedAPIKey{}, err
		}
		return ports.IssuedAPIKey{Key: key, Secret: minted.Secret}, nil
	})
}

func (s *Service) ListAPIKeys(ctx context.Context, tenantRef string) ([]domains.APIKey, error) {
	t, err := resolveTenant(ctx, s.repo.Tenants(), tenantRef)
	if err != nil {
		return nil, err
	}
	return s.repo.APIKeys().ListByTenant(ctx, t.ID)
}

func (s *Service) GetAPIKey(ctx context.Context, id uuid.UUID) (domains.APIKey, error) {
	k, err := s.repo.APIKeys().Get(ctx, id)
	if err != nil {
		return domains.APIKey{}, keyErr(err, id)
	}
	return k, nil
}

func (s *Service) UpdateAPIKey(
	ctx context.Context, w ports.Write, id uuid.UUID, p ports.APIKeyPatch,
) (domains.APIKey, error) {
	allowlist, err := validAllowlist(p.ModelAllowlist)
	if err != nil {
		return domains.APIKey{}, err
	}

	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.APIKey, error) {
		k, err := lockKey(ctx, tx, id)
		if err != nil {
			return domains.APIKey{}, err
		}
		if k.Revoked() {
			// A revoked key keeps the allowlist its old ledger rows were admitted
			// under.
			return domains.APIKey{}, fmt.Errorf("%w: api key %s is revoked", ports.ErrConflict, id)
		}
		if sameAllowlist(k.ModelAllowlist, allowlist) {
			return k, nil
		}

		k, err = tx.APIKeys().UpdateAllowlist(ctx, id, allowlist)
		if err != nil {
			return domains.APIKey{}, err
		}
		if err := s.audit(ctx, tx, w, "key.update", &k.TenantID, &k.ID, keyDetail(k)); err != nil {
			return domains.APIKey{}, err
		}
		return k, nil
	})
}

// RevokeAPIKey keeps the row, so the key stays attributable in the ledger rows
// it wrote.
func (s *Service) RevokeAPIKey(ctx context.Context, w ports.Write, id uuid.UUID) (domains.APIKey, error) {
	return mutate(ctx, s, func(ctx context.Context, tx ports.AdminTx) (domains.APIKey, error) {
		k, err := lockKey(ctx, tx, id)
		if err != nil || k.Revoked() {
			return k, err
		}

		k, err = tx.APIKeys().Revoke(ctx, id)
		if err != nil {
			return domains.APIKey{}, err
		}
		if err := s.audit(ctx, tx, w, "key.revoke", &k.TenantID, &k.ID, keyDetail(k)); err != nil {
			return domains.APIKey{}, err
		}
		return k, nil
	})
}

// lockKey locks the key's tenant, then re-reads the key under that lock: every
// key mutation takes the same tenant lock, so only the second read is the state
// this transaction will act on.
func lockKey(ctx context.Context, tx ports.AdminTx, id uuid.UUID) (domains.APIKey, error) {
	k, err := tx.APIKeys().Get(ctx, id)
	if err != nil {
		return domains.APIKey{}, keyErr(err, id)
	}
	if _, err := tx.Tenants().Lock(ctx, k.TenantID); err != nil {
		return domains.APIKey{}, err
	}
	k, err = tx.APIKeys().Get(ctx, id)
	if err != nil {
		return domains.APIKey{}, keyErr(err, id)
	}
	return k, nil
}

func keyErr(err error, id uuid.UUID) error {
	if errors.Is(err, ports.ErrNotFound) {
		return fmt.Errorf("%w: api key %s", ports.ErrNotFound, id)
	}
	return err
}

// validAllowlist rejects blank and duplicate aliases. It deliberately does not
// check them against the configured models: a customer may set up a tier before
// the alias it names is deployed.
func validAllowlist(list []string) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	if len(list) > maxAllowlistLen {
		return nil, invalid("model_allowlist may name at most %d aliases", maxAllowlistLen)
	}

	seen := make(map[string]bool, len(list))
	for _, alias := range list {
		switch {
		case strings.TrimSpace(alias) == "":
			return nil, invalid("model_allowlist must not contain blank aliases")
		case len(alias) > maxAliasLen:
			return nil, invalid("model_allowlist aliases must be at most %d bytes", maxAliasLen)
		case seen[alias]:
			return nil, invalid("model_allowlist names %q twice", alias)
		}
		seen[alias] = true
	}
	return list, nil
}

func sameAllowlist(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return slices.Equal(a, b)
}

func keyDetail(k domains.APIKey) gen.APIKeyAuditDetail {
	allowlist := nullable.NewNullNullable[gen.ModelAllowlist]()
	if k.ModelAllowlist != nil {
		allowlist = nullable.NewNullableWithValue(gen.ModelAllowlist(k.ModelAllowlist))
	}
	return gen.APIKeyAuditDetail{
		KeyPrefix:      k.Prefix,
		ModelAllowlist: allowlist,
		Revoked:        k.Revoked(),
	}
}
