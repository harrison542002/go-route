package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// AdminKeyPrefix is deliberately not KeyPrefix: a leaked token should be
// recognisable as the operator's credential rather than a tenant's, so a secret
// scanner can escalate accordingly.
const AdminKeyPrefix = "gr_admin_"

const MaxCredentialNameLen = 128

// validCredentialName is the character set that survives a log line, a shell
// and a URL unchanged, so audit_log's actor reads the same everywhere.
var validCredentialName = regexp.MustCompile(`^[A-Za-z0-9._@:+-]{1,128}$`)

// Credentials manages the admin API's own credentials. It is reached only from
// the CLI, never from the API: minting a credential from a credential is a
// privilege-escalation surface to design on purpose, not acquire by accident.
type Credentials struct {
	repo   ports.AdminCredentialRepository
	now    func() time.Time
	random io.Reader
}

// NewCredentials builds the use case; a nil random means crypto/rand.
func NewCredentials(repo ports.AdminCredentialRepository, now func() time.Time, random io.Reader) *Credentials {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Credentials{repo: repo, now: now, random: random}
}

// Create mints a credential. The returned secret is the only copy that will
// ever exist: the store keeps its SHA-256.
func (c *Credentials) Create(
	ctx context.Context, w ports.Write, spec ports.NewAdminCredential,
) (ports.IssuedAdminCredential, error) {
	if !validCredentialName.MatchString(spec.Name) {
		return ports.IssuedAdminCredential{}, invalid(
			"name must be 1-%d characters of letters, digits and ._@:+-", MaxCredentialNameLen)
	}
	// An unnamed role is refused rather than defaulted: a credential that can
	// change quotas because a field was left out is the accident roles prevent.
	if !spec.Role.Valid() {
		return ports.IssuedAdminCredential{}, invalid(
			"role must be admin or readonly, got %q", spec.Role)
	}

	minted, err := Mint(AdminKeyPrefix, c.random)
	if err != nil {
		return ports.IssuedAdminCredential{}, err
	}
	id, err := newID()
	if err != nil {
		return ports.IssuedAdminCredential{}, err
	}

	cred := domains.AdminCredential{
		ID: id, Name: spec.Name, Prefix: minted.Prefix,
		Role: spec.Role, ExpiresAt: spec.ExpiresAt,
	}
	entry, err := c.auditEntry(w, "admin_credential.create", cred)
	if err != nil {
		return ports.IssuedAdminCredential{}, err
	}

	cred, err = c.repo.CreateAdminCredential(ctx, cred, minted.Hash[:], entry)
	if err != nil {
		return ports.IssuedAdminCredential{}, err
	}
	return ports.IssuedAdminCredential{Credential: cred, Secret: minted.Secret}, nil
}

func (c *Credentials) List(ctx context.Context) ([]domains.AdminCredential, error) {
	return c.repo.ListAdminCredentials(ctx)
}

// Revoke kills a credential by name or id. The row stays, so the audit rows it
// wrote remain attributable; revoking twice is a no-op.
func (c *Credentials) Revoke(
	ctx context.Context, w ports.Write, ref string,
) (domains.AdminCredential, error) {
	cred, err := c.repo.AdminCredentialByRef(ctx, ref)
	if err != nil {
		return domains.AdminCredential{}, credentialErr(err, ref)
	}
	if cred.Revoked() {
		return cred, nil
	}

	entry, err := c.auditEntry(w, "admin_credential.revoke", cred)
	if err != nil {
		return domains.AdminCredential{}, err
	}
	return c.repo.RevokeAdminCredential(ctx, cred.ID, entry)
}

// auditEntry carries no tenant and no key id: a credential belongs to the
// operator, not to a tenant, so those columns stay NULL.
func (c *Credentials) auditEntry(
	w ports.Write, action string, cred domains.AdminCredential,
) (ports.AuditEntry, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return ports.AuditEntry{}, fmt.Errorf("admin: audit id: %w", err)
	}
	detail, err := json.Marshal(credentialDetail{
		CredentialID: cred.ID.String(),
		Name:         cred.Name,
		TokenPrefix:  cred.Prefix,
		Role:         string(cred.Role),
		ExpiresAt:    cred.ExpiresAt,
	})
	if err != nil {
		return ports.AuditEntry{}, fmt.Errorf("admin: encode audit detail: %w", err)
	}
	return ports.AuditEntry{
		ID:     id,
		At:     c.now(),
		Actor:  w.Actor,
		Action: action,
		Detail: string(detail),
	}, nil
}

// credentialDetail is the state a credential mutation leaves behind, for
// audit_log.reason_detail. The secret is absent, as everywhere it could have
// been written down. Role and expiry are part of that state: a reader asking
// why a write succeeded needs the role it was minted with, not today's.
type credentialDetail struct {
	CredentialID string     `json:"credential_id"`
	Name         string     `json:"name"`
	TokenPrefix  string     `json:"token_prefix"`
	Role         string     `json:"role"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

func credentialErr(err error, ref string) error {
	if errors.Is(err, ports.ErrNotFound) {
		return fmt.Errorf("%w: admin credential %q", ports.ErrNotFound, ref)
	}
	return err
}
