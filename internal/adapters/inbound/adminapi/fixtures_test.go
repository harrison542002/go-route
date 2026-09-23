package adminapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/ports"
)

var testUser = domains.AdminUser{
	ID:        uuid.MustParse("0192a000-0000-7000-8000-0000000000a1"),
	Email:     "ops@example.com",
	Role:      domains.RoleAdmin,
	CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
}

// users stands in for the Postgres user store the auth middleware reads on
// every request.
type users struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domains.AdminUser
	err  error
}

var _ ports.AdminUserAuthenticator = (*users)(nil)

func newUsers(list ...domains.AdminUser) *users {
	rows := make(map[uuid.UUID]domains.AdminUser, len(list))
	for _, u := range list {
		rows[u.ID] = u
	}
	return &users{rows: rows}
}

func (u *users) AdminUserByID(_ context.Context, id uuid.UUID) (domains.AdminUser, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.err != nil {
		return domains.AdminUser{}, u.err
	}
	user, ok := u.rows[id]
	if !ok {
		return domains.AdminUser{}, ports.ErrNotFound
	}
	return user, nil
}

// testIssuer signs the access tokens the middleware tests present. The secret
// is a fixture and long enough to pass the floor.
func testIssuer(t *testing.T) *tokens.Issuer {
	t.Helper()

	issuer, err := tokens.NewIssuer([]byte(testToken), "go-route-admin-test", 15*time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func accessTokenFor(t *testing.T, issuer *tokens.Issuer, user domains.AdminUser) string {
	t.Helper()

	token, _, err := issuer.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
