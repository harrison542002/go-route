package domains

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrInvalidAdminUser = errors.New("admin user: invalid")

// MaxEmailLen is what the API and the CLI accept, well above any address
// in use and well below anything that makes a log line unreadable.
const MaxEmailLen = 254

// AdminUser is a person who reaches the internal admin API, as against
// the machines AdminCredential holds. The password is never stored: only
// an argon2id hash of it, which never leaves the repository.
type AdminUser struct {
	ID uuid.UUID

	// Email is the identity and the login name, always lower-cased.
	Email string

	Role Role

	CreatedAt         time.Time
	DisabledAt        *time.Time
	LastLoginAt       *time.Time
	PasswordChangedAt time.Time
}

func (u AdminUser) Disabled() bool { return u.DisabledAt != nil }

// TokenIssuedAfterPasswordChange reports whether an access token minted at
// issued still speaks for this person. Changing a password ends every
// session, and a stateless access token would otherwise outlive that
// promise by its whole TTL.
func (u AdminUser) TokenIssuedAfterPasswordChange(issued time.Time) bool {
	return !issued.Truncate(time.Second).Before(u.PasswordChangedAt.Truncate(time.Second))
}

// Actor is the string written to audit_log.actor for every mutation this
// person makes. The prefix is what tells a reader which kind of identity
// acted: user: for a person, admin: for a machine credential.
func (u AdminUser) Actor() string { return "user:" + u.Email }

// NormaliseEmail lower-cases and trims an address so that one person has
// one spelling everywhere -- the unique index, the login lookup and the
// audit rows alike. Every path into the store goes through it; the
// column's CHECK is what catches one that does not.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail accepts a bare address, not a display name and address:
// "Ops <ops@example.com>" parses, and would then be an audit identity
// nobody could log in as.
func ValidateEmail(email string) error {
	if email == "" {
		return fmt.Errorf("%w: email is required", ErrInvalidAdminUser)
	}
	if len(email) > MaxEmailLen {
		return fmt.Errorf("%w: email must be at most %d characters", ErrInvalidAdminUser, MaxEmailLen)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return fmt.Errorf("%w: %q is not an email address", ErrInvalidAdminUser, email)
	}
	return nil
}

// AdminRefreshToken is the stored half of a person's session. The token
// itself is never stored, only its SHA-256.
type AdminRefreshToken struct {
	ID     uuid.UUID
	UserID uuid.UUID

	IssuedAt  time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time

	// ReplacedBy is the token a refresh rotated this one into, nil for
	// one that was never refreshed.
	ReplacedBy *uuid.UUID
}

func (t AdminRefreshToken) Revoked() bool { return t.RevokedAt != nil }

// Expired reports whether the token's lifetime has run out. The boundary
// is inclusive, as AdminCredential.Expired's is.
func (t AdminRefreshToken) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// Usable reports whether this token may still be exchanged for a new
// access token. Every reason it may not is refused identically by the
// API, so a client learns only that it has to log in again.
func (t AdminRefreshToken) Usable(now time.Time) bool { return !t.Revoked() && !t.Expired(now) }
