//go:generate mockgen -source=admin_users.go -destination=mocks/admin_users_mock.go -package=mocks

package ports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// AdminUserAuthenticator is the slice of the user store the admin API's
// auth middleware uses. The row is read on every request, so it -- and
// never a claim inside an access token -- is what a request is
// authorised against.
type AdminUserAuthenticator interface {
	// AdminUserByID returns ErrNotFound when no row has that id. A
	// disabled person is returned like any other -- refusing them is the
	// caller's job, so unknown and disabled take one path.
	AdminUserByID(ctx context.Context, id uuid.UUID) (domains.AdminUser, error)
}

// AdminUserRepository is where the people who reach the admin API live,
// along with the stored half of their sessions.
type AdminUserRepository interface {
	AdminUserAuthenticator

	// AdminUserByEmail expects an already normalised address, and carries
	// no password hash: only AdminUserForLogin does, so the hash reaches
	// the one caller that checks it and nothing that gets logged.
	AdminUserByEmail(ctx context.Context, email string) (domains.AdminUser, error)

	// AdminUserForLogin reads the person and their stored hash together,
	// because checking a password needs both and two lookups would be two
	// chances to answer differently.
	AdminUserForLogin(ctx context.Context, email string) (user domains.AdminUser, passwordHash string, err error)

	ListAdminUsers(ctx context.Context) ([]domains.AdminUser, error)

	// CreateAdminUser writes the person and their audit row in one
	// transaction. ErrConflict when the email is taken.
	CreateAdminUser(
		ctx context.Context, u domains.AdminUser, passwordHash string, e AuditEntry,
	) (domains.AdminUser, error)

	// DisableAdminUser sets disabled_at and revokes every refresh token
	// the person holds, with the audit row, in one transaction. Disabling
	// an already disabled person writes nothing.
	DisableAdminUser(ctx context.Context, id uuid.UUID, at time.Time, e AuditEntry) (domains.AdminUser, error)

	// SetAdminUserPassword replaces the hash and revokes every refresh
	// token the person holds, so a password change ends the sessions
	// opened under the old one.
	SetAdminUserPassword(
		ctx context.Context, id uuid.UUID, passwordHash string, at time.Time, e AuditEntry,
	) (domains.AdminUser, error)

	// StartAdminSession records a login: the refresh token, the last-login
	// stamp and the audit row, in one transaction.
	StartAdminSession(ctx context.Context, s NewAdminSession, e AuditEntry) error

	// AdminSessionByTokenHash returns the refresh token and the person it
	// belongs to, ErrNotFound when no row has that hash. A revoked,
	// expired or disabled one is returned like any other.
	AdminSessionByTokenHash(ctx context.Context, tokenHash []byte) (AdminSession, error)

	// RotateAdminSession revokes the presented token, issues its
	// replacement and records the link between them, in one transaction.
	// ErrConflict when the presented token was already revoked, which is
	// the race between two refreshes and is the caller's to interpret.
	RotateAdminSession(ctx context.Context, presented uuid.UUID, s NewAdminSession) error

	// EndAdminSession revokes one refresh token, with its audit row.
	// Revoking an already revoked token writes nothing.
	EndAdminSession(ctx context.Context, id uuid.UUID, at time.Time, e AuditEntry) error

	// RevokeAdminSessions cuts every refresh token a person holds, with
	// its audit row. It is what a replayed token triggers.
	RevokeAdminSessions(ctx context.Context, userID uuid.UUID, at time.Time, e AuditEntry) error

	// WriteAudit records something that changed nothing, which is what a
	// failed login is: there is no row to write it beside.
	WriteAudit(ctx context.Context, e AuditEntry) error
}

// NewAdminSession is one refresh token about to be stored. The plaintext
// never reaches the repository: only its SHA-256.
type NewAdminSession struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// AdminSession is a stored refresh token with its owner, read together
// because every decision the refresh endpoint makes needs both.
type AdminSession struct {
	Token domains.AdminRefreshToken
	User  domains.AdminUser
}

// NewAdminUser is what the CLI asks for when it creates a person. Role
// has no meaningful zero value: nothing may acquire admin by omission.
type NewAdminUser struct {
	Email string
	Role  domains.Role

	// Password is empty for one the CLI should generate.
	Password string
}

// CreatedAdminUser carries the generated password back to the CLI, which
// prints it once. It is empty when the caller supplied one.
type CreatedAdminUser struct {
	User     domains.AdminUser
	Password string
}

// Credentials is an email and a password as presented to the login
// endpoint, with the address of whoever presented them.
type Credentials struct {
	Email    string
	Password string

	// ClientIP is what the failure throttle counts against, alongside the
	// email. It is empty when the address could not be read, which counts
	// as one more client rather than as none.
	ClientIP string
}

// AdminAuth is the session half of the admin API: what a person does
// with a password, and what their client does with a refresh token.
type AdminAuth interface {
	// Login exchanges a password for a session. Every failure -- unknown
	// email, wrong password, disabled person, too many recent attempts --
	// returns ErrUnauthenticated and takes comparable time.
	Login(ctx context.Context, c Credentials) (AdminSessionTokens, error)

	// Refresh rotates a refresh token into a new session. Every failure
	// returns ErrUnauthenticated. Presenting an already revoked token is
	// treated as theft: the person's whole set is cut.
	Refresh(ctx context.Context, refreshToken string) (AdminSessionTokens, error)

	// Logout revokes the presented refresh token. An unknown or already
	// revoked one is not an error: the client is asking for the token to
	// be dead, and it is.
	Logout(ctx context.Context, actor, refreshToken string) error
}

// AdminSessionTokens is what a login or a refresh hands back. The two
// plaintexts exist here and nowhere else.
type AdminSessionTokens struct {
	AccessToken  string
	ExpiresAt    time.Time
	RefreshToken string
	User         domains.AdminUser
}
