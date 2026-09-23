//go:generate mockgen -source=admin_credentials.go -destination=mocks/admin_credentials_mock.go -package=mocks

package ports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// AdminAuthenticator is the slice of the credential store the admin API's auth
// middleware uses. It cannot mint or revoke: credentials are managed from the CLI.
type AdminAuthenticator interface {
	// AdminCredentialByTokenHash returns ErrNotFound when no row has that hash.
	// A revoked credential is returned like any other -- refusing it is the
	// caller's job, so unknown and revoked take one path and the response cannot
	// tell them apart.
	AdminCredentialByTokenHash(ctx context.Context, tokenHash []byte) (domains.AdminCredential, error)

	// TouchAdminCredential is best-effort: a stale last-used stamp is fine, so a
	// failure is logged and never reaches the request.
	TouchAdminCredential(ctx context.Context, id uuid.UUID, at time.Time) error
}

// AdminCredentialRepository is where the admin API's own credentials live. Only
// the CLI writes through it.
type AdminCredentialRepository interface {
	AdminAuthenticator

	// AdminCredentialByRef finds a credential by name or id, ErrNotFound when
	// neither matches.
	AdminCredentialByRef(ctx context.Context, ref string) (domains.AdminCredential, error)

	ListAdminCredentials(ctx context.Context) ([]domains.AdminCredential, error)

	// CreateAdminCredential writes the credential and its audit row in one
	// transaction. ErrConflict when the name is taken.
	CreateAdminCredential(
		ctx context.Context, c domains.AdminCredential, tokenHash []byte, e AuditEntry,
	) (domains.AdminCredential, error)

	// RevokeAdminCredential sets revoked_at, with its audit row in the same
	// transaction. Revoking an already revoked credential writes nothing.
	RevokeAdminCredential(ctx context.Context, id uuid.UUID, e AuditEntry) (domains.AdminCredential, error)
}

// NewAdminCredential is what the CLI asks for when it mints a credential. Role
// has no meaningful zero value: nothing may acquire admin by omission.
type NewAdminCredential struct {
	Name string
	Role domains.Role

	// ExpiresAt is nil for a credential that does not expire.
	ExpiresAt *time.Time
}

// IssuedAdminCredential is the only place the plaintext of an admin token ever
// exists: the CLI prints it once and it is not recoverable.
type IssuedAdminCredential struct {
	Credential domains.AdminCredential
	Secret     string
}
