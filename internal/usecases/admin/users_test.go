package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/password"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/ports"
)

// userStore is an in-memory stand-in for the Postgres user repository. It
// keeps the password hashes and the sessions the use case writes, so a test
// can assert on what was stored rather than on what was returned.
type userStore struct {
	mu       sync.Mutex
	users    map[uuid.UUID]domains.AdminUser
	hashes   map[uuid.UUID]string
	sessions map[uuid.UUID]domains.AdminRefreshToken
	byHash   map[string]uuid.UUID
	audit    []ports.AuditEntry
	err      error

	// logins counts the reads Login makes. Verifying a password means
	// reading the stored hash first, so a login that never reads one
	// never derived anything either.
	logins int
}

var _ ports.AdminUserRepository = (*userStore)(nil)

func newUserStore() *userStore {
	return &userStore{
		users:    map[uuid.UUID]domains.AdminUser{},
		hashes:   map[uuid.UUID]string{},
		sessions: map[uuid.UUID]domains.AdminRefreshToken{},
		byHash:   map[string]uuid.UUID{},
	}
}

func (s *userStore) AdminUserByID(_ context.Context, id uuid.UUID) (domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[id]
	if !ok {
		return domains.AdminUser{}, ports.ErrNotFound
	}
	return user, nil
}

func (s *userStore) AdminUserByEmail(_ context.Context, email string) (domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, user := range s.users {
		if user.Email == email {
			return user, nil
		}
	}
	return domains.AdminUser{}, ports.ErrNotFound
}

func (s *userStore) AdminUserForLogin(ctx context.Context, email string) (domains.AdminUser, string, error) {
	s.mu.Lock()
	s.logins++
	s.mu.Unlock()

	user, err := s.AdminUserByEmail(ctx, email)
	if err != nil {
		return domains.AdminUser{}, "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return user, s.hashes[user.ID], nil
}

func (s *userStore) ListAdminUsers(context.Context) ([]domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]domains.AdminUser, 0, len(s.users))
	for _, user := range s.users {
		out = append(out, user)
	}
	return out, nil
}

func (s *userStore) CreateAdminUser(
	_ context.Context, u domains.AdminUser, hash string, e ports.AuditEntry,
) (domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return domains.AdminUser{}, s.err
	}
	for _, existing := range s.users {
		if existing.Email == u.Email {
			return domains.AdminUser{}, ports.ErrConflict
		}
	}
	s.users[u.ID], s.hashes[u.ID] = u, hash
	s.audit = append(s.audit, e)
	return u, nil
}

func (s *userStore) DisableAdminUser(
	_ context.Context, id uuid.UUID, at time.Time, e ports.AuditEntry,
) (domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.users[id]
	user.DisabledAt = &at
	s.users[id] = user
	s.revokeAll(id, at)
	s.audit = append(s.audit, e)
	return user, nil
}

func (s *userStore) SetAdminUserPassword(
	_ context.Context, id uuid.UUID, hash string, at time.Time, e ports.AuditEntry,
) (domains.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.users[id]
	user.PasswordChangedAt = at
	s.users[id], s.hashes[id] = user, hash
	s.revokeAll(id, at)
	s.audit = append(s.audit, e)
	return user, nil
}

func (s *userStore) StartAdminSession(_ context.Context, n ports.NewAdminSession, e ports.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.insert(n)
	user := s.users[n.UserID]
	user.LastLoginAt = &n.IssuedAt
	s.users[n.UserID] = user
	s.audit = append(s.audit, e)
	return nil
}

func (s *userStore) AdminSessionByTokenHash(_ context.Context, hash []byte) (ports.AdminSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.byHash[string(hash)]
	if !ok {
		return ports.AdminSession{}, ports.ErrNotFound
	}
	token := s.sessions[id]
	return ports.AdminSession{Token: token, User: s.users[token.UserID]}, nil
}

func (s *userStore) RotateAdminSession(_ context.Context, presented uuid.UUID, n ports.NewAdminSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	token := s.sessions[presented]
	if token.Revoked() {
		return ports.ErrConflict
	}
	token.RevokedAt, token.ReplacedBy = &n.IssuedAt, &n.ID
	s.sessions[presented] = token
	s.insert(n)
	return nil
}

func (s *userStore) EndAdminSession(_ context.Context, id uuid.UUID, at time.Time, e ports.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	token := s.sessions[id]
	if token.Revoked() {
		return nil
	}
	token.RevokedAt = &at
	s.sessions[id] = token
	s.audit = append(s.audit, e)
	return nil
}

func (s *userStore) RevokeAdminSessions(
	_ context.Context, userID uuid.UUID, at time.Time, e ports.AuditEntry,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.revokeAll(userID, at)
	s.audit = append(s.audit, e)
	return nil
}

func (s *userStore) WriteAudit(_ context.Context, e ports.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.audit = append(s.audit, e)
	return nil
}

func (s *userStore) insert(n ports.NewAdminSession) {
	s.sessions[n.ID] = domains.AdminRefreshToken{
		ID: n.ID, UserID: n.UserID, IssuedAt: n.IssuedAt, ExpiresAt: n.ExpiresAt,
	}
	s.byHash[string(n.TokenHash)] = n.ID
}

func (s *userStore) revokeAll(userID uuid.UUID, at time.Time) {
	for id, token := range s.sessions {
		if token.UserID == userID && !token.Revoked() {
			token.RevokedAt = &at
			s.sessions[id] = token
		}
	}
}

func (s *userStore) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.audit))
	for _, e := range s.audit {
		out = append(out, e.Action)
	}
	return out
}

func (s *userStore) detailFor(action string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.audit {
		if e.Action == action {
			return e.Detail
		}
	}
	return ""
}

func (s *userStore) liveSessions(userID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := 0
	for _, token := range s.sessions {
		if token.UserID == userID && !token.Revoked() {
			live++
		}
	}
	return live
}

const testPassword = "correct-horse-battery-staple"

func newTestUsers(t *testing.T) (*Users, *userStore) {
	t.Helper()

	issuer, err := tokens.NewIssuer([]byte("0123456789abcdef0123456789abcdef"),
		"go-route-admin-test", 15*time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	store := newUserStore()
	return NewUsers(store, issuer, DefaultRefreshTTL, time.Now, nil), store
}

func create(t *testing.T, users *Users, email string, role domains.Role) domains.AdminUser {
	t.Helper()

	created, err := users.Create(context.Background(), ports.Write{Actor: "cli:test"},
		ports.NewAdminUser{Email: email, Role: role, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	return created.User
}

func TestCreateStoresAHashAndNotThePassword(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "Ops@Example.com ", domains.RoleAdmin)

	if user.Email != "ops@example.com" {
		t.Errorf("email = %q; it must be normalised before it is stored", user.Email)
	}
	stored := store.hashes[user.ID]
	if !strings.HasPrefix(stored, "$argon2id$") || strings.Contains(stored, testPassword) {
		t.Errorf("password_hash = %q", stored)
	}
	if got := store.actions(); len(got) != 1 || got[0] != "user.create" {
		t.Errorf("audit = %v", got)
	}
}

func TestCreateGeneratesAPasswordWhenNoneIsGiven(t *testing.T) {
	users, _ := newTestUsers(t)

	created, err := users.Create(context.Background(), ports.Write{Actor: "cli:test"},
		ports.NewAdminUser{Email: "gen@example.com", Role: domains.RoleReadonly})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Password) < 32 {
		t.Fatalf("generated password = %q", created.Password)
	}

	session, err := users.Login(context.Background(),
		ports.Credentials{Email: "gen@example.com", Password: created.Password})
	if err != nil {
		t.Fatalf("the generated password does not log in: %v", err)
	}
	if session.User.Role != domains.RoleReadonly {
		t.Errorf("role = %q", session.User.Role)
	}
}

func TestCreateRefusesABadEmailOrRole(t *testing.T) {
	users, _ := newTestUsers(t)

	for name, spec := range map[string]ports.NewAdminUser{
		"no email":       {Role: domains.RoleAdmin, Password: testPassword},
		"not an email":   {Email: "ops", Role: domains.RoleAdmin, Password: testPassword},
		"display name":   {Email: "Ops <ops@example.com>", Role: domains.RoleAdmin, Password: testPassword},
		"no role":        {Email: "ops@example.com", Password: testPassword},
		"unknown role":   {Email: "ops@example.com", Role: "root", Password: testPassword},
		"short password": {Email: "ops@example.com", Role: domains.RoleAdmin, Password: "short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := users.Create(context.Background(), ports.Write{}, spec); !errors.Is(err, ports.ErrInvalid) {
				t.Errorf("Create = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLoginIssuesASessionAndAuditsIt(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	session, err := users.Login(context.Background(), ports.Credentials{
		Email: "OPS@example.com", Password: testPassword, ClientIP: "203.0.113.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case session.AccessToken == "":
		t.Error("no access token")
	case !strings.HasPrefix(session.RefreshToken, RefreshTokenPrefix):
		t.Errorf("refresh token = %q", session.RefreshToken)
	case !session.ExpiresAt.After(time.Now()):
		t.Errorf("expiry = %s", session.ExpiresAt)
	case store.users[user.ID].LastLoginAt == nil:
		t.Error("last_login_at was not written")
	}

	if got := store.actions(); len(got) != 2 || got[1] != "user.login" {
		t.Errorf("audit = %v", got)
	}
}

func TestLoginRefusesEveryFailureTheSameWay(t *testing.T) {
	disabledUsers, _ := newTestUsers(t)
	disabled := create(t, disabledUsers, "gone@example.com", domains.RoleAdmin)
	if _, err := disabledUsers.Disable(context.Background(), ports.Write{Actor: "cli:test"}, disabled.Email); err != nil {
		t.Fatal(err)
	}

	normal, _ := newTestUsers(t)
	create(t, normal, "ops@example.com", domains.RoleAdmin)

	for name, tc := range map[string]struct {
		users *Users
		creds ports.Credentials
	}{
		"unknown email":    {normal, ports.Credentials{Email: "nobody@example.com", Password: testPassword}},
		"wrong password":   {normal, ports.Credentials{Email: "ops@example.com", Password: "not-the-password"}},
		"empty password":   {normal, ports.Credentials{Email: "ops@example.com"}},
		"disabled person":  {disabledUsers, ports.Credentials{Email: "gone@example.com", Password: testPassword}},
		"empty everything": {normal, ports.Credentials{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tc.users.Login(context.Background(), tc.creds); !errors.Is(err, ports.ErrUnauthenticated) {
				t.Errorf("Login = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestFailedLoginIsAuditedWithoutThePassword(t *testing.T) {
	users, store := newTestUsers(t)
	create(t, users, "ops@example.com", domains.RoleAdmin)

	_, err := users.Login(context.Background(), ports.Credentials{
		Email: "ops@example.com", Password: "hunter2-is-my-password", ClientIP: "203.0.113.7",
	})
	if !errors.Is(err, ports.ErrUnauthenticated) {
		t.Fatal(err)
	}

	detail := store.detailFor("user.login_failed")
	switch {
	case detail == "":
		t.Fatal("no user.login_failed row")
	case !strings.Contains(detail, "ops@example.com"):
		t.Errorf("detail does not name the email attempted: %s", detail)
	case !strings.Contains(detail, "203.0.113.7"):
		t.Errorf("detail does not name the address: %s", detail)
	case strings.Contains(detail, "hunter2"):
		t.Fatalf("the password was written to the audit log: %s", detail)
	}
}

func TestRepeatedFailuresAreThrottled(t *testing.T) {
	users, _ := newTestUsers(t)
	create(t, users, "ops@example.com", domains.RoleAdmin)

	wrong := ports.Credentials{Email: "ops@example.com", Password: "wrong", ClientIP: "203.0.113.7"}
	for range MaxLoginFailures {
		if _, err := users.Login(context.Background(), wrong); !errors.Is(err, ports.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}

	right := ports.Credentials{Email: "ops@example.com", Password: testPassword, ClientIP: "203.0.113.7"}
	if _, err := users.Login(context.Background(), right); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Error("the right password was accepted while the pair was throttled")
	}

	// The count is per email and address, so another client is unaffected.
	elsewhere := right
	elsewhere.ClientIP = "198.51.100.4"
	if _, err := users.Login(context.Background(), elsewhere); err != nil {
		t.Errorf("another address was throttled too: %v", err)
	}
}

func TestRefreshRotatesAndKillsThePresentedToken(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	first, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}

	second, err := users.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case second.RefreshToken == first.RefreshToken:
		t.Error("the refresh token was not rotated")
	case second.AccessToken == "":
		t.Error("no access token")
	case store.liveSessions(user.ID) != 1:
		t.Errorf("live sessions = %d, want 1", store.liveSessions(user.ID))
	}

	if _, err := users.Refresh(context.Background(), second.RefreshToken); err != nil {
		t.Errorf("the rotated token does not work: %v", err)
	}
}

// A refresh token presented after it was rotated away means two clients hold
// it, so everything that person has is cut.
func TestReplayingARefreshTokenRevokesTheWholeSet(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	first, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	second, err := users.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := users.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Fatalf("a replayed token was accepted: %v", err)
	}
	if store.liveSessions(user.ID) != 0 {
		t.Errorf("live sessions = %d, want 0", store.liveSessions(user.ID))
	}
	if _, err := users.Refresh(context.Background(), second.RefreshToken); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Errorf("the rotated token survived the replay: %v", err)
	}

	if store.detailFor("user.refresh_reuse") == "" {
		t.Error("no user.refresh_reuse row")
	}
}

func TestRefreshRefusesEveryFailureTheSameWay(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	session, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}

	expired, _ := newTestUsers(t)
	expiredUser := create(t, expired, "old@example.com", domains.RoleAdmin)
	expired.refreshTTL = time.Nanosecond
	expiredSession, err := expired.Login(context.Background(),
		ports.Credentials{Email: expiredUser.Email, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		users *Users
		token string
		setup func()
	}{
		"unknown token": {users, "gr_refresh_nothing", nil},
		"empty token":   {users, "", nil},
		"expired token": {expired, expiredSession.RefreshToken, nil},
		"disabled person": {users, session.RefreshToken, func() {
			at := time.Now()
			u := store.users[user.ID]
			u.DisabledAt = &at
			store.users[user.ID] = u
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			if _, err := tc.users.Refresh(context.Background(), tc.token); !errors.Is(err, ports.ErrUnauthenticated) {
				t.Errorf("Refresh = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestLogoutRevokesTheTokenAndIsAudited(t *testing.T) {
	users, store := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	session, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}

	if err := users.Logout(context.Background(), user.Actor(), session.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if store.liveSessions(user.ID) != 0 {
		t.Error("the session survived a logout")
	}
	if store.detailFor("user.logout") == "" {
		t.Error("no user.logout row")
	}
	if _, err := users.Refresh(context.Background(), session.RefreshToken); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Errorf("a logged-out token still refreshes: %v", err)
	}

	// Logging out an unknown token is not an error: the caller asked for it
	// to be dead, and it is.
	if err := users.Logout(context.Background(), user.Actor(), "gr_refresh_nothing"); err != nil {
		t.Errorf("Logout = %v", err)
	}
}

func TestDisableAndPasswordChangeEndEverySession(t *testing.T) {
	for name, tc := range map[string]struct {
		act    func(*Users, domains.AdminUser) error
		action string
	}{
		"disable": {
			func(u *Users, user domains.AdminUser) error {
				_, err := u.Disable(context.Background(), ports.Write{Actor: "cli:test"}, user.Email)
				return err
			},
			"user.disable",
		},
		"password change": {
			func(u *Users, user domains.AdminUser) error {
				_, err := u.SetPassword(context.Background(), ports.Write{Actor: "cli:test"}, user.Email, "")
				return err
			},
			"user.password_change",
		},
	} {
		t.Run(name, func(t *testing.T) {
			users, store := newTestUsers(t)
			user := create(t, users, "ops@example.com", domains.RoleAdmin)

			session, err := users.Login(context.Background(),
				ports.Credentials{Email: user.Email, Password: testPassword})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.act(users, user); err != nil {
				t.Fatal(err)
			}

			if store.liveSessions(user.ID) != 0 {
				t.Errorf("live sessions = %d, want 0", store.liveSessions(user.ID))
			}
			if _, err := users.Refresh(context.Background(), session.RefreshToken); !errors.Is(err, ports.ErrUnauthenticated) {
				t.Errorf("Refresh = %v, want ErrUnauthenticated", err)
			}
			if store.detailFor(tc.action) == "" {
				t.Errorf("no %s row", tc.action)
			}
		})
	}
}

func TestSetPasswordReplacesTheOldOne(t *testing.T) {
	users, _ := newTestUsers(t)
	user := create(t, users, "ops@example.com", domains.RoleAdmin)

	changed, err := users.SetPassword(context.Background(), ports.Write{Actor: "cli:test"}, user.Email, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: testPassword}); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Error("the old password still logs in")
	}
	if _, err := users.Login(context.Background(),
		ports.Credentials{Email: user.Email, Password: changed.Password, ClientIP: "198.51.100.9"}); err != nil {
		t.Errorf("the new password does not log in: %v", err)
	}
}

func TestDisableAnUnknownPerson(t *testing.T) {
	users, _ := newTestUsers(t)

	if _, err := users.Disable(context.Background(), ports.Write{}, "nobody@example.com"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Disable = %v, want ErrNotFound", err)
	}
}

func TestCreateAndSetPasswordApplyThePolicy(t *testing.T) {
	for name, tc := range map[string]struct{ plain, rule string }{
		"too short":    {"tiny", "at least"},
		"over the cap": {strings.Repeat("a", password.MaxLength+1), "at most"},
		"all spaces":   {strings.Repeat(" ", 20), "entirely whitespace"},
		"the address":  {"ops-is-my-passphrase", "must not contain the address"},
		"product name": {"go-route-forever-and-ever", "name of this product"},
	} {
		t.Run(name, func(t *testing.T) {
			users, store := newTestUsers(t)

			_, err := users.Create(context.Background(), ports.Write{Actor: "cli:test"},
				ports.NewAdminUser{Email: "ops@example.com", Role: domains.RoleAdmin, Password: tc.plain})
			assertPolicyRefusal(t, err, tc.plain, tc.rule)
			if len(store.users) != 0 {
				t.Fatal("the user was created with a refused password")
			}

			user := create(t, users, "ops@example.com", domains.RoleAdmin)
			was := store.hashes[user.ID]

			_, err = users.SetPassword(context.Background(), ports.Write{Actor: "cli:test"}, user.Email, tc.plain)
			assertPolicyRefusal(t, err, tc.plain, tc.rule)
			if store.hashes[user.ID] != was {
				t.Error("the stored hash was replaced by a refused password")
			}
		})
	}
}

func assertPolicyRefusal(t *testing.T, err error, plain, rule string) {
	t.Helper()

	if !errors.Is(err, ports.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), rule) {
		t.Errorf("err = %q; it does not say which rule failed (%q)", err, rule)
	}
	if strings.TrimSpace(plain) != "" && strings.Contains(err.Error(), plain) {
		t.Errorf("err = %q quotes the password", err)
	}
}

// The generated password is what almost every account gets, so it has to
// pass the policy that a typed one is held to.
func TestGeneratedPasswordSatisfiesThePolicy(t *testing.T) {
	users, _ := newTestUsers(t)

	for i := range 20 {
		created, err := users.Create(context.Background(), ports.Write{Actor: "cli:test"},
			ports.NewAdminUser{
				Email: fmt.Sprintf("generated-person-%d@example.com", i),
				Role:  domains.RoleAdmin,
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := password.Check(created.Password, created.User.Email); err != nil {
			t.Fatalf("the generated password is refused by the policy: %v", err)
		}
	}
}

func TestLoginRefusesAnOverLongPasswordBeforeHashing(t *testing.T) {
	users, store := newTestUsers(t)
	create(t, users, "ops@example.com", domains.RoleAdmin)

	_, over := users.Login(context.Background(), ports.Credentials{
		Email: "ops@example.com", Password: strings.Repeat("a", password.MaxLength+1), ClientIP: "203.0.113.7",
	})
	if !errors.Is(over, ports.ErrUnauthenticated) {
		t.Fatalf("Login = %v, want ErrUnauthenticated", over)
	}

	// Verifying reads the stored hash. No read means no derivation, which
	// is the whole point of the cap: an unauthenticated caller must not be
	// able to buy argon2 work with a long string.
	if store.logins != 0 {
		t.Errorf("the stored hash was read %d times", store.logins)
	}

	_, wrong := users.Login(context.Background(), ports.Credentials{
		Email: "ops@example.com", Password: "not-the-password", ClientIP: "203.0.113.7",
	})
	if over.Error() != wrong.Error() {
		t.Errorf("over-long = %v, wrong password = %v; they must be the same answer", over, wrong)
	}
	if store.logins != 1 {
		t.Errorf("hash reads = %d; a password within the cap must be verified", store.logins)
	}
}
