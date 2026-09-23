package tokens

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var (
	secret   = []byte("0123456789abcdef0123456789abcdef")
	testNow  = time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	testUser = domains.AdminUser{
		ID:    uuid.MustParse("0192a000-0000-7000-8000-0000000000a1"),
		Email: "ops@example.com",
		Role:  domains.RoleReadonly,
	}
)

func newTestIssuer(t *testing.T, at time.Time) *Issuer {
	t.Helper()

	issuer, err := NewIssuer(secret, "go-route-admin", 15*time.Minute, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	issuer := newTestIssuer(t, testNow)

	token, issued, err := issuer.Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}

	claims, err := issuer.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case claims.UserID != testUser.ID:
		t.Errorf("subject = %s", claims.UserID)
	case claims.Email != testUser.Email:
		t.Errorf("email = %q", claims.Email)
	case claims.Role != testUser.Role:
		t.Errorf("role = %q", claims.Role)
	case claims.ID == "" || claims.ID != issued.ID:
		t.Errorf("jti = %q, issued %q", claims.ID, issued.ID)
	case !claims.Expiry.Equal(testNow.Add(15 * time.Minute)):
		t.Errorf("expiry = %s", claims.Expiry)
	}
}

func TestTwoTokensGetDifferentIDs(t *testing.T) {
	issuer := newTestIssuer(t, testNow)

	_, first, err := issuer.Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := issuer.Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Error("two tokens issued in the same second share a jti")
	}
}

func TestVerifyRefusesAnExpiredToken(t *testing.T) {
	token, _, err := newTestIssuer(t, testNow).Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly at the expiry the token is already over.
	for name, at := range map[string]time.Time{
		"a second late": testNow.Add(15*time.Minute + time.Second),
		"an hour late":  testNow.Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newTestIssuer(t, at).Verify(token); !errors.Is(err, ErrExpired) {
				t.Errorf("Verify = %v, want ErrExpired", err)
			}
		})
	}

	if _, err := newTestIssuer(t, testNow.Add(14*time.Minute)).Verify(token); err != nil {
		t.Errorf("a token inside its lifetime was refused: %v", err)
	}
}

func TestVerifyRefusesABadSignature(t *testing.T) {
	token, _, err := newTestIssuer(t, testNow).Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}

	other, err := NewIssuer([]byte("fedcba9876543210fedcba9876543210"), "go-route-admin",
		15*time.Minute, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token signed with another secret verified: %v", err)
	}

	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + parts[1] + ".AAAA" + parts[2][4:]
	if _, err := newTestIssuer(t, testNow).Verify(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a tampered signature verified: %v", err)
	}
}

// Algorithm confusion is the failure this package exists to avoid: a token
// whose header names none, or an asymmetric algorithm the verifier would then
// feed its own key to as an HMAC secret, must not verify.
func TestVerifyRefusesAnotherAlgorithm(t *testing.T) {
	claims := jwt.RegisteredClaims{
		Subject:   testUser.ID.String(),
		Issuer:    "go-route-admin",
		IssuedAt:  jwt.NewNumericDate(testNow),
		ExpiresAt: jwt.NewNumericDate(testNow.Add(time.Hour)),
	}

	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}

	issuer := newTestIssuer(t, testNow)
	for name, token := range map[string]string{"alg none": none, "alg HS512": hs512} {
		t.Run(name, func(t *testing.T) {
			if _, err := issuer.Verify(token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyRefusesAnotherIssuer(t *testing.T) {
	elsewhere, err := NewIssuer(secret, "someone-else", 15*time.Minute, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := elsewhere.Issue(testUser)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := newTestIssuer(t, testNow).Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token from another issuer verified: %v", err)
	}
}

func TestVerifyRefusesGarbage(t *testing.T) {
	issuer := newTestIssuer(t, testNow)

	for name, token := range map[string]string{
		"empty":        "",
		"one segment":  "abcdef",
		"two segments": "abc.def",
		"not base64":   "!!!.???.###",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := issuer.Verify(token); err == nil {
				t.Error("garbage verified")
			}
		})
	}
}

func TestNewIssuerRefusesAWeakConfiguration(t *testing.T) {
	now := func() time.Time { return testNow }

	for name, tc := range map[string]struct {
		secret []byte
		issuer string
		ttl    time.Duration
	}{
		"short secret": {[]byte("too-short"), "go-route-admin", time.Minute},
		"no secret":    {nil, "go-route-admin", time.Minute},
		"no issuer":    {secret, "", time.Minute},
		"ttl too low":  {secret, "go-route-admin", time.Second},
		"ttl too high": {secret, "go-route-admin", 48 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIssuer(tc.secret, tc.issuer, tc.ttl, now); err == nil {
				t.Error("a weak configuration was accepted")
			}
		})
	}
}
