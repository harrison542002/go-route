package ports

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var (
	ErrNoCredentials   = errors.New("auth: no credentials presented")
	ErrUnknownKey      = errors.New("auth: unrecognised api key")
	ErrKeyRevoked      = errors.New("auth: api key revoked")
	ErrTenantDisabled  = errors.New("auth: tenant disabled")
	ErrModelNotAllowed = errors.New("auth: model not allowed for this key")
)

// Identity is who a request is acting as, established from its API key
// before anything else happens.
//
// The tenant is derived here and nowhere else. A client never names the
// tenant it wants to bill; it presents a credential, and the credential
// decides. That is the whole reason this sits in front of the router.
type Identity struct {
	Tenant domains.Tenant

	// KeyID attributes the spend to one credential, so revoking a leaked
	// key does not require guessing which traffic was its.
	KeyID uuid.UUID

	// Allowlist is the set of model aliases this key may request. Nil
	// allows every alias; empty allows none.
	Allowlist []string
}

// AllowsModel reports whether this key may request an alias.
func (i Identity) AllowsModel(alias string) bool {
	if i.Allowlist == nil {
		return true
	}
	return slices.Contains(i.Allowlist, alias)
}

// Authenticator turns a presented API key into an Identity.
type Authenticator interface {
	Authenticate(ctx context.Context, presented string) (Identity, error)
}
