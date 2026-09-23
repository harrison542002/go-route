package tokens

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

const MinSecretBytes = 32

const (
	MinAccessTTL = time.Minute
	MaxAccessTTL = 24 * time.Hour
)

var (
	ErrInvalidToken = errors.New("tokens: invalid access token")
	ErrExpired      = errors.New("tokens: access token expired")
)

type Claims struct {
	UserID uuid.UUID
	Email  string
	Role   domains.Role
	ID     string
	Issued time.Time
	Expiry time.Time
}

type accessClaims struct {
	jwt.RegisteredClaims

	Email string       `json:"email"`
	Role  domains.Role `json:"role"`
}

// Issuer signs and verifies access tokens with one HMAC secret.
type Issuer struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewIssuer(secret []byte, issuer string, ttl time.Duration, now func() time.Time) (*Issuer, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("tokens: secret must be at least %d bytes, got %d", MinSecretBytes, len(secret))
	}
	if issuer == "" {
		return nil, errors.New("tokens: an issuer is required")
	}
	if ttl < MinAccessTTL || ttl > MaxAccessTTL {
		return nil, fmt.Errorf("tokens: access token ttl must be between %s and %s, got %s",
			MinAccessTTL, MaxAccessTTL, ttl)
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{secret: secret, issuer: issuer, ttl: ttl, now: now}, nil
}

func (i *Issuer) TTL() time.Duration {
	return i.ttl
}

// Issue signs an access token for a user.
func (i *Issuer) Issue(user domains.AdminUser) (string, Claims, error) {
	jti, err := uuid.NewV7()
	if err != nil {
		return "", Claims{}, fmt.Errorf("tokens: jti: %w", err)
	}

	issued := i.now().UTC().Truncate(time.Second)
	claims := Claims{
		UserID: user.ID,
		Email:  user.Email,
		Role:   user.Role,
		ID:     jti.String(),
		Issued: issued,
		Expiry: issued.Add(i.ttl),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   claims.UserID.String(),
			Issuer:    i.issuer,
			ID:        claims.ID,
			IssuedAt:  jwt.NewNumericDate(claims.Issued),
			ExpiresAt: jwt.NewNumericDate(claims.Expiry),
		},
		Email: claims.Email,
		Role:  claims.Role,
	}).SignedString(i.secret)
	if err != nil {
		return "", Claims{}, fmt.Errorf("tokens: sign: %w", err)
	}
	return signed, claims, nil
}

// Verify checks a presented token and returns what it claims.
func (i *Issuer) Verify(token string) (Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &accessClaims{},
		func(*jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(i.now),
	)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return Claims{}, ErrExpired
	case err != nil:
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*accessClaims)
	if !ok || claims.ExpiresAt == nil || claims.IssuedAt == nil {
		return Claims{}, fmt.Errorf("%w: incomplete claims", ErrInvalidToken)
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: subject is not a user id", ErrInvalidToken)
	}

	return Claims{
		UserID: userID,
		Email:  claims.Email,
		Role:   claims.Role,
		ID:     claims.ID,
		Issued: claims.IssuedAt.Time,
		Expiry: claims.ExpiresAt.Time,
	}, nil
}
