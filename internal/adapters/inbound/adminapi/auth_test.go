package adminapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var testCredential = domains.AdminCredential{
	ID:     uuid.MustParse("0192a000-0000-7000-8000-0000000000c1"),
	Name:   "billing-sync",
	Prefix: "gr_admin_4k2xq7",
	Role:   domains.RoleAdmin,
}

// credentials stands in for the Postgres credential store. It is hand-written
// rather than generated because the touch happens on its own goroutine and the
// test has to be able to wait for it.
type credentials struct {
	mu   sync.Mutex
	rows map[[sha256.Size]byte]domains.AdminCredential
	err  error

	touched chan uuid.UUID
}

var _ ports.AdminAuthenticator = (*credentials)(nil)

func newCredentials(token string, cred domains.AdminCredential) *credentials {
	return &credentials{
		rows:    map[[sha256.Size]byte]domains.AdminCredential{sha256.Sum256([]byte(token)): cred},
		touched: make(chan uuid.UUID, 8),
	}
}

func (c *credentials) AdminCredentialByTokenHash(
	_ context.Context, tokenHash []byte,
) (domains.AdminCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return domains.AdminCredential{}, c.err
	}
	cred, ok := c.rows[[sha256.Size]byte(tokenHash)]
	if !ok {
		return domains.AdminCredential{}, ports.ErrNotFound
	}
	return cred, nil
}

func (c *credentials) TouchAdminCredential(_ context.Context, id uuid.UUID, _ time.Time) error {
	select {
	case c.touched <- id:
	default:
	}
	return nil
}

// serveAuth runs one request through RequireToken alone and reports the status
// and the actor the next handler would have seen.
func serveAuth(t *testing.T, creds ports.AdminAuthenticator, authorization string) (int, string) {
	t.Helper()
	code, actor, _, _ := serveAuthenticated(t, creds, authorization, http.MethodDelete)
	return code, actor
}

// serveAuthenticated is serveAuth, also reporting the role and the body.
func serveAuthenticated(
	t *testing.T, creds ports.AdminAuthenticator, authorization, method string,
) (int, string, domains.Role, string) {
	t.Helper()

	var (
		actor string
		role  domains.Role
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		actor, role = actorFrom(r.Context()), roleFrom(r.Context())
	})

	req := httptest.NewRequest(method, "/admin/v1/tenants/acme", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	RequireToken(creds, newUsers(testUser), testIssuer(t))(next).ServeHTTP(rec, req)

	return rec.Code, actor, role, rec.Body.String()
}

func TestRequireTokenResolvesTheCredential(t *testing.T) {
	creds := newCredentials(testToken, testCredential)

	code, actor := serveAuth(t, creds, "Bearer "+testToken)
	if code != http.StatusOK || actor != "admin:billing-sync" {
		t.Fatalf("code = %d, actor = %q", code, actor)
	}

	select {
	case id := <-creds.touched:
		if id != testCredential.ID {
			t.Errorf("touched %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("last_used_at was never touched")
	}
}

func TestRequireTokenRefusesEverythingElseIdentically(t *testing.T) {
	revoked := testCredential
	at := time.Now()
	revoked.RevokedAt = &at

	expired := testCredential
	past := time.Now().Add(-time.Minute)
	expired.ExpiresAt = &past

	// A credential that expires at this very instant is over.
	justExpired := testCredential
	now := time.Now()
	justExpired.ExpiresAt = &now

	broken := newCredentials(testToken, testCredential)
	broken.err = errors.New("connection refused")

	// Every refusal must be the same answer, byte for byte.
	_, _, _, want := serveAuthenticated(t, newCredentials("other", testCredential),
		"Bearer "+testToken, http.MethodDelete)

	for name, tc := range map[string]struct {
		creds         ports.AdminAuthenticator
		authorization string
	}{
		"expired token":       {newCredentials(testToken, expired), "Bearer " + testToken},
		"expiring right now":  {newCredentials(testToken, justExpired), "Bearer " + testToken},
		"revoked and expired": {newCredentials(testToken, expiredAndRevoked()), "Bearer " + testToken},
		"no header":           {newCredentials(testToken, testCredential), ""},
		"not bearer":          {newCredentials(testToken, testCredential), "Basic " + testToken},
		"garbage":             {newCredentials(testToken, testCredential), "Bearer not-a-token"},
		"empty token":         {newCredentials(testToken, testCredential), "Bearer "},
		"token prefix":        {newCredentials(testToken, testCredential), "Bearer " + testToken[:10]},
		"unknown token":       {newCredentials("some-other-token", testCredential), "Bearer " + testToken},
		"revoked token":       {newCredentials(testToken, revoked), "Bearer " + testToken},
		"store is down":       {broken, "Bearer " + testToken},
		"lowercase word":      {newCredentials("other", testCredential), "bearer " + testToken},
	} {
		t.Run(name, func(t *testing.T) {
			code, actor, _, body := serveAuthenticated(t, tc.creds, tc.authorization, http.MethodDelete)
			if code != http.StatusUnauthorized || actor != "" {
				t.Fatalf("code = %d, actor = %q; want 401 and no handler run", code, actor)
			}
			if body != want {
				t.Errorf("body = %q, want %q; refusals must be indistinguishable", body, want)
			}
		})
	}
}

func expiredAndRevoked() domains.AdminCredential {
	cred := testCredential
	at := time.Now().Add(-time.Hour)
	cred.ExpiresAt, cred.RevokedAt = &at, &at
	return cred
}

func TestExpiredCredentialIsNotTouched(t *testing.T) {
	expired := testCredential
	past := time.Now().Add(-time.Minute)
	expired.ExpiresAt = &past
	creds := newCredentials(testToken, expired)

	if code, _ := serveAuth(t, creds, "Bearer "+testToken); code != http.StatusUnauthorized {
		t.Fatalf("code = %d", code)
	}
	select {
	case id := <-creds.touched:
		t.Errorf("touched %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnexpiredCredentialAuthenticatesWithItsRole(t *testing.T) {
	cred := testCredential
	cred.Role = domains.RoleReadonly
	future := time.Now().Add(time.Hour)
	cred.ExpiresAt = &future

	code, actor, role, _ := serveAuthenticated(t, newCredentials(testToken, cred),
		"Bearer "+testToken, http.MethodGet)
	if code != http.StatusOK || actor != "admin:billing-sync" || role != domains.RoleReadonly {
		t.Fatalf("code = %d, actor = %q, role = %q", code, actor, role)
	}
}

func TestRevokedCredentialIsNotTouched(t *testing.T) {
	revoked := testCredential
	at := time.Now()
	revoked.RevokedAt = &at
	creds := newCredentials(testToken, revoked)

	if code, _ := serveAuth(t, creds, "Bearer "+testToken); code != http.StatusUnauthorized {
		t.Fatalf("code = %d", code)
	}
	select {
	case id := <-creds.touched:
		t.Errorf("touched %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestActorWithoutAuthentication(t *testing.T) {
	if actor := actorFrom(context.Background()); actor != "admin:unknown" {
		t.Errorf("actor = %q", actor)
	}
}

func TestNewServerNeedsACredentialStore(t *testing.T) {
	if _, err := NewServer("", NewHandler(nil, nil, nil, nil),
		nil, newUsers(), testIssuer(t), time.Second); err == nil {
		t.Error("a server with no credential store was built")
	}
}

func TestNewServerNeedsAUserStoreAndAVerifier(t *testing.T) {
	creds := newCredentials(testToken, testCredential)

	if _, err := NewServer("", NewHandler(nil, nil, nil, nil),
		creds, nil, testIssuer(t), time.Second); err == nil {
		t.Error("a server with no user store was built")
	}
	if _, err := NewServer("", NewHandler(nil, nil, nil, nil),
		creds, newUsers(), nil, time.Second); err == nil {
		t.Error("a server with no access token verifier was built")
	}
}
