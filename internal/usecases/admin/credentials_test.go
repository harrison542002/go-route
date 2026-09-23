package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// credentialRepo records what the use case asked of it; the repository itself
// is exercised against a real database in tests/integration.
type credentialRepo struct {
	rows []domains.AdminCredential

	created     domains.AdminCredential
	createdHash []byte
	revokedID   uuid.UUID
	entries     []ports.AuditEntry

	createErr error
	refErr    error
}

var _ ports.AdminCredentialRepository = (*credentialRepo)(nil)

func (s *credentialRepo) AdminCredentialByTokenHash(
	context.Context, []byte,
) (domains.AdminCredential, error) {
	return domains.AdminCredential{}, ports.ErrNotFound
}

func (s *credentialRepo) TouchAdminCredential(context.Context, uuid.UUID, time.Time) error {
	return nil
}

func (s *credentialRepo) AdminCredentialByRef(_ context.Context, ref string) (domains.AdminCredential, error) {
	if s.refErr != nil {
		return domains.AdminCredential{}, s.refErr
	}
	for _, c := range s.rows {
		if c.Name == ref || c.ID.String() == ref {
			return c, nil
		}
	}
	return domains.AdminCredential{}, ports.ErrNotFound
}

func (s *credentialRepo) ListAdminCredentials(context.Context) ([]domains.AdminCredential, error) {
	return s.rows, nil
}

func (s *credentialRepo) CreateAdminCredential(
	_ context.Context, c domains.AdminCredential, tokenHash []byte, e ports.AuditEntry,
) (domains.AdminCredential, error) {
	if s.createErr != nil {
		return domains.AdminCredential{}, s.createErr
	}
	s.created, s.createdHash = c, tokenHash
	s.entries = append(s.entries, e)
	c.CreatedAt = testNow
	return c, nil
}

func (s *credentialRepo) RevokeAdminCredential(
	_ context.Context, id uuid.UUID, e ports.AuditEntry,
) (domains.AdminCredential, error) {
	s.revokedID = id
	s.entries = append(s.entries, e)

	for _, c := range s.rows {
		if c.ID == id {
			at := testNow
			c.RevokedAt = &at
			return c, nil
		}
	}
	return domains.AdminCredential{}, ports.ErrNotFound
}

func newCredentials(t *testing.T) (*Credentials, *credentialRepo) {
	t.Helper()
	store := &credentialRepo{}
	return NewCredentials(store, func() time.Time { return testNow }, bytes.NewReader(make([]byte, 64))), store
}

// billingSync is what `create-token --name billing-sync` hands the use case.
var billingSync = ports.NewAdminCredential{Name: "billing-sync", Role: domains.RoleAdmin}

func TestCreateCredentialStoresOnlyTheHash(t *testing.T) {
	creds, store := newCredentials(t)

	issued, err := creds.Create(context.Background(), ports.Write{Actor: "cli:alice"}, billingSync)
	if err != nil {
		t.Fatal(err)
	}

	wantSecret := AdminKeyPrefix + strings.Repeat("a", 52)
	if issued.Secret != wantSecret {
		t.Errorf("secret = %q", issued.Secret)
	}
	if issued.Credential.Name != "billing-sync" || issued.Credential.Prefix != AdminKeyPrefix+"aaaaaa" {
		t.Errorf("credential = %+v", issued.Credential)
	}
	if issued.Credential.ID == uuid.Nil {
		t.Error("no id was minted")
	}

	sum := sha256.Sum256([]byte(wantSecret))
	if !bytes.Equal(store.createdHash, sum[:]) {
		t.Errorf("stored hash = %x, want %x", store.createdHash, sum)
	}
	if strings.Contains(store.entries[0].Detail, strings.Repeat("a", 52)) {
		t.Errorf("the secret reached the audit row: %s", store.entries[0].Detail)
	}
}

func TestCreateCredentialAuditsTheCaller(t *testing.T) {
	creds, store := newCredentials(t)

	issued, err := creds.Create(context.Background(), ports.Write{Actor: "cli:alice"}, billingSync)
	if err != nil {
		t.Fatal(err)
	}

	e := store.entries[0]
	if e.Actor != "cli:alice" || e.Action != "admin_credential.create" || e.At != testNow {
		t.Errorf("entry = %+v", e)
	}
	if e.TenantID != nil || e.KeyID != nil {
		t.Error("a credential belongs to no tenant and is not an api key")
	}
	if !strings.Contains(e.Detail, `"name":"billing-sync"`) ||
		!strings.Contains(e.Detail, issued.Credential.ID.String()) {
		t.Errorf("detail = %s", e.Detail)
	}
}

func TestCreateCredentialRejectsUnusableNames(t *testing.T) {
	for name, given := range map[string]string{
		"empty":     "",
		"spaces":    "billing sync",
		"newline":   "billing\nsync",
		"too long":  strings.Repeat("n", MaxCredentialNameLen+1),
		"quotes":    `"admin"`,
		"non-ascii": "billing-sync-é",
	} {
		t.Run(name, func(t *testing.T) {
			creds, store := newCredentials(t)
			spec := ports.NewAdminCredential{Name: given, Role: domains.RoleAdmin}
			_, err := creds.Create(context.Background(), ports.Write{Actor: "cli"}, spec)
			if !errors.Is(err, ports.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if store.created.Name != "" {
				t.Error("a credential was stored anyway")
			}
		})
	}
}

func TestCreateCredentialRejectsAnUnnamedOrUnknownRole(t *testing.T) {
	for name, role := range map[string]domains.Role{
		"absent":  "",
		"unknown": "superuser",
		"cased":   "Admin",
	} {
		t.Run(name, func(t *testing.T) {
			creds, store := newCredentials(t)
			spec := ports.NewAdminCredential{Name: "billing-sync", Role: role}

			_, err := creds.Create(context.Background(), ports.Write{Actor: "cli"}, spec)
			if !errors.Is(err, ports.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if store.created.Name != "" {
				t.Error("a credential was stored anyway")
			}
		})
	}
}

func TestCreateCredentialCarriesRoleAndExpiry(t *testing.T) {
	expires := testNow.Add(90 * 24 * time.Hour)

	for _, spec := range []ports.NewAdminCredential{
		{Name: "dashboard", Role: domains.RoleReadonly, ExpiresAt: &expires},
		{Name: "dashboard", Role: domains.RoleReadonly},
	} {
		creds, store := newCredentials(t)

		issued, err := creds.Create(context.Background(), ports.Write{Actor: "cli"}, spec)
		if err != nil {
			t.Fatal(err)
		}
		if issued.Credential.Role != domains.RoleReadonly {
			t.Errorf("role = %q", issued.Credential.Role)
		}
		if !sameInstant(store.created.ExpiresAt, spec.ExpiresAt) {
			t.Errorf("stored expiry = %v, want %v", store.created.ExpiresAt, spec.ExpiresAt)
		}

		detail := store.entries[0].Detail
		if !strings.Contains(detail, `"role":"readonly"`) {
			t.Errorf("detail does not carry the role: %s", detail)
		}
		wantExpiry := `"expires_at":null`
		if spec.ExpiresAt != nil {
			wantExpiry = `"expires_at":"` + expires.UTC().Format(time.RFC3339Nano)
		}
		if !strings.Contains(detail, wantExpiry) {
			t.Errorf("detail does not carry the expiry: %s", detail)
		}
	}
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func TestRevokeCredentialByNameAndID(t *testing.T) {
	cred := domains.AdminCredential{
		ID: uuid.MustParse("0192a000-0000-7000-8000-0000000000c1"), Name: "billing-sync",
	}

	for _, ref := range []string{cred.Name, cred.ID.String()} {
		creds, store := newCredentials(t)
		store.rows = []domains.AdminCredential{cred}

		got, err := creds.Revoke(context.Background(), ports.Write{Actor: "cli:alice"}, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if got.RevokedAt == nil || store.revokedID != cred.ID {
			t.Errorf("%s: revoked %+v", ref, got)
		}
		if e := store.entries[0]; e.Action != "admin_credential.revoke" || e.Actor != "cli:alice" {
			t.Errorf("%s: entry = %+v", ref, e)
		}
	}
}

func TestRevokeCredentialTwiceWritesNothing(t *testing.T) {
	at := testNow
	creds, store := newCredentials(t)
	store.rows = []domains.AdminCredential{{
		ID: uuid.MustParse("0192a000-0000-7000-8000-0000000000c1"), Name: "old", RevokedAt: &at,
	}}

	got, err := creds.Revoke(context.Background(), ports.Write{Actor: "cli"}, "old")
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if len(store.entries) != 0 || store.revokedID != uuid.Nil {
		t.Error("a second revocation was written")
	}
}

func TestRevokeUnknownCredentialSaysWhichOne(t *testing.T) {
	creds, _ := newCredentials(t)

	_, err := creds.Revoke(context.Background(), ports.Write{Actor: "cli"}, "never-existed")
	if !errors.Is(err, ports.ErrNotFound) || !strings.Contains(err.Error(), "never-existed") {
		t.Fatalf("err = %v", err)
	}
}

func TestMint(t *testing.T) {
	for _, prefix := range []string{KeyPrefix, AdminKeyPrefix} {
		minted, err := Mint(prefix, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		switch {
		case !strings.HasPrefix(minted.Secret, prefix):
			t.Errorf("%q: secret = %q", prefix, minted.Secret)
		case len(minted.Secret) != len(prefix)+52: // 256 bits of base32
			t.Errorf("%q: secret is %d characters", prefix, len(minted.Secret))
		case minted.Prefix != minted.Secret[:len(prefix)+displayChars]:
			t.Errorf("%q: display prefix = %q", prefix, minted.Prefix)
		case minted.Hash != sha256.Sum256([]byte(minted.Secret)):
			t.Errorf("%q: the hash is not of the secret", prefix)
		}

		again, err := Mint(prefix, rand.Reader)
		if err != nil || again.Secret == minted.Secret {
			t.Errorf("%q: minted the same secret twice", prefix)
		}
	}
}

func TestMintFailsWhenRandomnessDoes(t *testing.T) {
	if _, err := Mint(AdminKeyPrefix, bytes.NewReader([]byte("too short"))); err == nil {
		t.Error("a secret was minted from exhausted randomness")
	}
}
