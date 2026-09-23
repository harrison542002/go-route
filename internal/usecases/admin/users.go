package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/password"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/ports"
)

const RefreshTokenPrefix = "gr_refresh_"
const DefaultRefreshTTL = 30 * 24 * time.Hour

// Users is the people half of the admin API: the accounts the CLI
// manages, and the sessions the API hands out.
type Users struct {
	repo       ports.AdminUserRepository
	issuer     *tokens.Issuer
	refreshTTL time.Duration
	now        func() time.Time
	random     io.Reader
	throttle   *throttle
}

var _ ports.AdminAuth = (*Users)(nil)

// NewUsers builds the use case. issuer may be nil for the CLI, which
// manages accounts and never issues a token; a nil random means
// crypto/rand and a zero refreshTTL means DefaultRefreshTTL.
func NewUsers(
	repo ports.AdminUserRepository, issuer *tokens.Issuer,
	refreshTTL time.Duration, now func() time.Time, random io.Reader,
) *Users {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	if refreshTTL <= 0 {
		refreshTTL = DefaultRefreshTTL
	}
	return &Users{
		repo: repo, issuer: issuer, refreshTTL: refreshTTL,
		now: now, random: random, throttle: newThrottle(),
	}
}

// Create makes a person. A password is generated when none is given, and
// the plaintext in the result is the only copy that will ever exist.
func (u *Users) Create(
	ctx context.Context, w ports.Write, spec ports.NewAdminUser,
) (ports.CreatedAdminUser, error) {
	email := domains.NormaliseEmail(spec.Email)
	if err := domains.ValidateEmail(email); err != nil {
		return ports.CreatedAdminUser{}, invalid("%s", errors.Unwrap(err))
	}

	if !spec.Role.Valid() {
		return ports.CreatedAdminUser{}, invalid("role must be admin or readonly, got %q", spec.Role)
	}

	plain, generated, err := u.choosePassword(spec.Password, email)
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	hash, err := password.Hash(plain)
	if err != nil {
		return ports.CreatedAdminUser{}, fmt.Errorf("admin: hash password: %w", err)
	}

	id, err := newID()
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	user := domains.AdminUser{ID: id, Email: email, Role: spec.Role, PasswordChangedAt: u.now()}
	entry, err := u.auditEntry(w.Actor, "user.create", userDetail(user))
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	user, err = u.repo.CreateAdminUser(ctx, user, hash, entry)
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}
	return ports.CreatedAdminUser{User: user, Password: generated}, nil
}

func (u *Users) List(ctx context.Context) ([]domains.AdminUser, error) {
	return u.repo.ListAdminUsers(ctx)
}

// Disable locks a person out. The row stays, so the audit rows they wrote
// remain attributable; disabling twice is a no-op.
func (u *Users) Disable(ctx context.Context, w ports.Write, email string) (domains.AdminUser, error) {
	user, err := u.byEmail(ctx, email)
	if err != nil {
		return domains.AdminUser{}, err
	}
	if user.Disabled() {
		return user, nil
	}

	entry, err := u.auditEntry(w.Actor, "user.disable", userDetail(user))
	if err != nil {
		return domains.AdminUser{}, err
	}
	return u.repo.DisableAdminUser(ctx, user.ID, u.now(), entry)
}

// SetPassword replaces a person's password and ends every session opened
// under the old one.
func (u *Users) SetPassword(
	ctx context.Context, w ports.Write, email, plain string,
) (ports.CreatedAdminUser, error) {
	user, err := u.byEmail(ctx, email)
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	plain, generated, err := u.choosePassword(plain, user.Email)
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	hash, err := password.Hash(plain)
	if err != nil {
		return ports.CreatedAdminUser{}, fmt.Errorf("admin: hash password: %w", err)
	}

	entry, err := u.auditEntry(w.Actor, "user.password_change", userDetail(user))
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}

	user, err = u.repo.SetAdminUserPassword(ctx, user.ID, hash, u.now(), entry)
	if err != nil {
		return ports.CreatedAdminUser{}, err
	}
	return ports.CreatedAdminUser{User: user, Password: generated}, nil
}

// choosePassword returns the plaintext to hash and the copy to print,
// which is empty when the caller supplied the password and so already has
// it.
//
// The policy runs over a supplied password only. A generated one is 52
// base32 characters from crypto/rand, so checking it would buy nothing
// and would leave a vanishingly rare refusal for an operator to puzzle
// over; TestGeneratedPasswordSatisfiesThePolicy is what holds that line.
func (u *Users) choosePassword(supplied, email string) (plain, generated string, err error) {
	if supplied == "" {
		minted, err := Mint("", u.random)
		if err != nil {
			return "", "", err
		}
		return minted.Secret, minted.Secret, nil
	}

	var policy password.PolicyError
	if err := password.Check(supplied, email); errors.As(err, &policy) {
		return "", "", invalid("password %s", policy.Rule)
	} else if err != nil {
		return "", "", err
	}
	return supplied, "", nil
}

// Login exchanges a password for a session.
func (u *Users) Login(ctx context.Context, c ports.Credentials) (ports.AdminSessionTokens, error) {
	email := domains.NormaliseEmail(c.Email)
	now := u.now()
	key := email + "\x00" + c.ClientIP

	if u.throttle.blocked(key, now) {
		u.auditLoginFailure(ctx, email, c.ClientIP, "throttled")
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}

	// No stored password can be over the cap, so one that is cannot be
	// right and is refused before anything is looked up or derived --
	// otherwise an unauthenticated caller could spend a 64 MiB argon2
	// derivation per request for the price of a long string. It is not an
	// oracle: the answer is the same one every other failure gives.
	if password.TooLong(c.Password) {
		u.throttle.fail(key, now)
		u.auditLoginFailure(ctx, email, c.ClientIP, "password_over_cap")
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}

	user, hash, err := u.repo.AdminUserForLogin(ctx, email)
	if errors.Is(err, ports.ErrNotFound) {
		hash = password.Dummy()
	} else if err != nil {
		return ports.AdminSessionTokens{}, err
	}

	badPassword := password.Verify(hash, c.Password) != nil
	if badPassword || user.ID == uuid.Nil || user.Disabled() {
		u.throttle.fail(key, now)
		u.auditLoginFailure(ctx, email, c.ClientIP, "invalid_credentials")
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}

	u.throttle.succeed(key)
	return u.startSession(ctx, user, now)
}

// Refresh rotates a refresh token into a new session.
func (u *Users) Refresh(ctx context.Context, refreshToken string) (ports.AdminSessionTokens, error) {
	now := u.now()

	session, err := u.repo.AdminSessionByTokenHash(ctx, hashSecret(refreshToken))
	if errors.Is(err, ports.ErrNotFound) {
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}

	if session.Token.Revoked() {
		u.auditSessionReuse(ctx, session, now)
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}
	if session.Token.Expired(now) || session.User.Disabled() {
		return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
	}

	access, expiry, err := u.issue(session.User)
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}
	minted, next, err := u.mintSession(session.User.ID, now)
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}

	if err := u.repo.RotateAdminSession(ctx, session.Token.ID, next); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return ports.AdminSessionTokens{}, ports.ErrUnauthenticated
		}
		return ports.AdminSessionTokens{}, err
	}

	return ports.AdminSessionTokens{
		AccessToken: access, ExpiresAt: expiry,
		RefreshToken: minted.Secret, User: session.User,
	}, nil
}

// Logout revokes the presented refresh token.
func (u *Users) Logout(ctx context.Context, actor, refreshToken string) error {
	session, err := u.repo.AdminSessionByTokenHash(ctx, hashSecret(refreshToken))
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if actor == "" {
		actor = session.User.Actor()
	}

	entry, err := u.auditEntry(actor, "user.logout", userDetail(session.User))
	if err != nil {
		return err
	}
	return u.repo.EndAdminSession(ctx, session.Token.ID, u.now(), entry)
}

func (u *Users) startSession(
	ctx context.Context, user domains.AdminUser, now time.Time,
) (ports.AdminSessionTokens, error) {
	access, expiry, err := u.issue(user)
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}
	minted, session, err := u.mintSession(user.ID, now)
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}

	entry, err := u.auditEntry(user.Actor(), "user.login", userDetail(user))
	if err != nil {
		return ports.AdminSessionTokens{}, err
	}
	if err := u.repo.StartAdminSession(ctx, session, entry); err != nil {
		return ports.AdminSessionTokens{}, err
	}

	return ports.AdminSessionTokens{
		AccessToken: access, ExpiresAt: expiry,
		RefreshToken: minted.Secret, User: user,
	}, nil
}

func (u *Users) issue(user domains.AdminUser) (string, time.Time, error) {
	if u.issuer == nil {
		return "", time.Time{}, errors.New("admin: no access token issuer configured")
	}
	access, claims, err := u.issuer.Issue(user)
	if err != nil {
		return "", time.Time{}, err
	}
	return access, claims.Expiry, nil
}

func (u *Users) mintSession(
	userID uuid.UUID, now time.Time,
) (Minted, ports.NewAdminSession, error) {
	minted, err := Mint(RefreshTokenPrefix, u.random)
	if err != nil {
		return Minted{}, ports.NewAdminSession{}, err
	}
	id, err := newID()
	if err != nil {
		return Minted{}, ports.NewAdminSession{}, err
	}
	return minted, ports.NewAdminSession{
		ID:        id,
		UserID:    userID,
		TokenHash: minted.Hash[:],
		IssuedAt:  now,
		ExpiresAt: now.Add(u.refreshTTL),
	}, nil
}

func (u *Users) byEmail(ctx context.Context, email string) (domains.AdminUser, error) {
	user, err := u.repo.AdminUserByEmail(ctx, domains.NormaliseEmail(email))
	if errors.Is(err, ports.ErrNotFound) {
		return domains.AdminUser{}, fmt.Errorf("%w: admin user %q", ports.ErrNotFound, email)
	}
	return user, err
}

func (u *Users) auditLoginFailure(ctx context.Context, email, clientIP, reason string) {
	entry, err := u.auditEntry("user:"+email, "user.login_failed", loginFailureDetail{
		Email: email, ClientIP: clientIP, Reason: reason,
	})
	if err == nil {
		err = u.repo.WriteAudit(ctx, entry)
	}
	if err != nil {
		logAuditFailure("user.login_failed", err)
	}
}

func (u *Users) auditSessionReuse(ctx context.Context, session ports.AdminSession, now time.Time) {
	entry, err := u.auditEntry(session.User.Actor(), "user.refresh_reuse", userDetail(session.User))
	if err == nil {
		err = u.repo.RevokeAdminSessions(ctx, session.User.ID, now, entry)
	}
	if err != nil {
		logAuditFailure("user.refresh_reuse", err)
	}
}

func (u *Users) auditEntry(actor, action string, detail any) (ports.AuditEntry, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return ports.AuditEntry{}, fmt.Errorf("admin: audit id: %w", err)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return ports.AuditEntry{}, fmt.Errorf("admin: encode audit detail: %w", err)
	}
	return ports.AuditEntry{ID: id, At: u.now(), Actor: actor, Action: action, Detail: string(raw)}, nil
}

func userDetail(u domains.AdminUser) adminUserDetail {
	return adminUserDetail{UserID: u.ID.String(), Email: u.Email, Role: string(u.Role)}
}

type adminUserDetail struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

type loginFailureDetail struct {
	Email    string `json:"email"`
	ClientIP string `json:"client_ip,omitempty"`
	Reason   string `json:"reason"`
}
