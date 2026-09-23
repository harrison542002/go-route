package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/ports"
)

// sessions stands in for the login use case. It records what it was asked and
// answers with whatever the test set.
type sessions struct {
	login    ports.Credentials
	loggedIn ports.AdminSessionTokens
	err      error

	logoutActor string
	logoutToken string
}

var _ ports.AdminAuth = (*sessions)(nil)

func (s *sessions) Login(_ context.Context, c ports.Credentials) (ports.AdminSessionTokens, error) {
	s.login = c
	return s.loggedIn, s.err
}

func (s *sessions) Refresh(_ context.Context, _ string) (ports.AdminSessionTokens, error) {
	return s.loggedIn, s.err
}

func (s *sessions) Logout(_ context.Context, actor, token string) error {
	s.logoutActor, s.logoutToken = actor, token
	return s.err
}

// serveWithUsers runs one request through the whole chain the server builds,
// with both credential stores behind it.
func serveWithUsers(
	t *testing.T, store *users, auth ports.AdminAuth, req *http.Request,
) *httptest.ResponseRecorder {
	t.Helper()

	server, err := NewServer("", NewHandler(nil, auth, nil, func() time.Time { return testNow }),
		newCredentials(testToken, testCredential), store, testIssuer(t), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	return rec
}

func whoami(t *testing.T, store *users, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/auth/me", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return serveWithUsers(t, store, &sessions{}, req)
}

func TestJWTAndMachineTokenBothAuthenticate(t *testing.T) {
	issuer := testIssuer(t)
	store := newUsers(testUser)

	person := whoami(t, store, "Bearer "+accessTokenFor(t, issuer, testUser))
	if person.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", person.Code, person.Body)
	}

	var got struct {
		Actor string
		Kind  string
		Role  string
		User  *struct{ Email string }
	}
	if err := json.Unmarshal(person.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	switch {
	case got.Actor != "user:ops@example.com":
		t.Errorf("actor = %q", got.Actor)
	case got.Kind != "user":
		t.Errorf("kind = %q", got.Kind)
	case got.User == nil || got.User.Email != testUser.Email:
		t.Errorf("user = %+v", got.User)
	}

	machine := whoami(t, store, "Bearer "+testToken)
	if machine.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", machine.Code, machine.Body)
	}
	got.User = nil
	if err := json.Unmarshal(machine.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	switch {
	case got.Actor != "admin:billing-sync":
		t.Errorf("actor = %q", got.Actor)
	case got.Kind != "machine":
		t.Errorf("kind = %q", got.Kind)
	case got.User != nil:
		t.Errorf("a machine credential was given a user: %+v", got.User)
	}
}

// The role a request is authorised against is the one on the row, read this
// request. A token issued before a demotion must not still carry the old one.
func TestRoleComesFromTheRowNotTheToken(t *testing.T) {
	issuer := testIssuer(t)
	token := accessTokenFor(t, issuer, testUser)

	demoted := testUser
	demoted.Role = domains.RoleReadonly

	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/tenants/acme", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := serveWithUsers(t, newUsers(demoted), &sessions{}, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s; a demoted person kept their old role", rec.Code, rec.Body)
	}

	rec = whoami(t, newUsers(demoted), "Bearer "+token)
	if !strings.Contains(rec.Body.String(), `"role":"readonly"`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestReadonlyPersonMayNotWrite(t *testing.T) {
	readonly := testUser
	readonly.Role = domains.RoleReadonly
	token := accessTokenFor(t, testIssuer(t), readonly)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/tenants", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)

	rec := serveWithUsers(t, newUsers(readonly), &sessions{}, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "permission_error") {
		t.Errorf("body = %s", rec.Body)
	}
}

// A readonly person still has a session to end, so logout is the one change
// the role check lets through.
func TestReadonlyPersonMayLogOut(t *testing.T) {
	readonly := testUser
	readonly.Role = domains.RoleReadonly
	token := accessTokenFor(t, testIssuer(t), readonly)

	auth := &sessions{}
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/logout",
		strings.NewReader(`{"refresh_token":"gr_refresh_abc"}`))
	req.Header.Set("Authorization", "Bearer "+token)

	rec := serveWithUsers(t, newUsers(readonly), auth, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if auth.logoutActor != readonly.Actor() || auth.logoutToken != "gr_refresh_abc" {
		t.Errorf("logout(%q, %q)", auth.logoutActor, auth.logoutToken)
	}
}

func TestLogoutStillNeedsAToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/logout",
		strings.NewReader(`{"refresh_token":"gr_refresh_abc"}`))

	if rec := serveWithUsers(t, newUsers(testUser), &sessions{}, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
}

// Every way a person's token can be refused answers exactly as a machine
// token's refusals do, so the two kinds cannot be told apart either.
func TestPersonRefusalsAreIndistinguishable(t *testing.T) {
	issuer := testIssuer(t)
	good := accessTokenFor(t, issuer, testUser)

	disabled := testUser
	at := time.Now()
	disabled.DisabledAt = &at

	elsewhere, err := tokens.NewIssuer([]byte("fedcba9876543210fedcba9876543210"),
		"go-route-admin-test", time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	expiredIssuer, err := tokens.NewIssuer([]byte(testToken), "go-route-admin-test", time.Minute,
		func() time.Time { return time.Now().Add(-time.Hour) })
	if err != nil {
		t.Fatal(err)
	}

	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Subject:   testUser.ID.String(),
		Issuer:    "go-route-admin-test",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	broken := newUsers(testUser)
	broken.err = errors.New("connection refused")

	want := whoami(t, newUsers(testUser), "Bearer "+testToken+"-nope").Body.String()

	for name, tc := range map[string]struct {
		store *users
		token string
	}{
		"disabled person":  {newUsers(disabled), good},
		"no such person":   {newUsers(), good},
		"store is down":    {broken, good},
		"other secret":     {newUsers(testUser), accessTokenFor(t, elsewhere, testUser)},
		"expired token":    {newUsers(testUser), accessTokenFor(t, expiredIssuer, testUser)},
		"alg none":         {newUsers(testUser), unsigned},
		"three dot pieces": {newUsers(testUser), "a.b.c"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := whoami(t, tc.store, "Bearer "+tc.token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
			}
			if rec.Body.String() != want {
				t.Errorf("body = %q, want %q; refusals must be indistinguishable", rec.Body, want)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}
}

func TestLoginPassesTheClientAddressToTheThrottle(t *testing.T) {
	auth := &sessions{err: ports.ErrUnauthenticated}

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/login",
		strings.NewReader(`{"email":"ops@example.com","password":"correct-horse-battery-staple"}`))
	req.RemoteAddr = "203.0.113.7:51234"
	// A header anyone can send must not reach the throttle.
	req.Header.Set("X-Forwarded-For", "198.51.100.1")

	rec := serveWithUsers(t, newUsers(testUser), auth, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if auth.login.ClientIP != "203.0.113.7" {
		t.Errorf("client ip = %q", auth.login.ClientIP)
	}
	if !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Errorf("body = %s", rec.Body)
	}
}
