// Package password hashes and checks the passwords admin users log in
// with. It is pure: no storage, no transport, no clock.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// The parameters every new hash is written with. They travel inside the
// encoded string, so raising them here leaves rows written under the old
// ones verifiable, and a password is rehashed when its owner next
// changes it rather than by a migration nobody can run.
const (
	timeCost    uint32 = 3
	memoryCost  uint32 = 64 * 1024
	parallelism uint8  = 2
	saltLength         = 16
	keyLength   uint32 = 32
)

// MinLength is the shortest password this package will hash. Length is
// the rule that reliably buys entropy; the rest of the policy is in
// Check, and the CLI generates a strong password by default.
const MinLength = 12

var (
	ErrMismatch  = errors.New("password: does not match")
	ErrMalformed = errors.New("password: malformed hash")
)

var b64 = base64.RawStdEncoding

// Hash returns a PHC-encoded argon2id hash of plain, with a salt drawn
// for this password alone. It enforces the length bounds itself, so no
// caller can derive over an unbounded input by forgetting Check.
func Hash(plain string) (string, error) {
	if err := checkLength(plain); err != nil {
		return "", err
	}

	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: salt: %w", err)
	}
	return encode(salt, derive(plain, salt, timeCost, memoryCost, parallelism, keyLength)), nil
}

// Verify reports whether plain produced encoded. It is constant-time
// against the stored digest, and it does the full derivation before
// comparing, so the time it takes says nothing about how nearly the
// password was right.
func Verify(encoded, plain string) error {
	t, m, p, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}

	got := derive(plain, salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// Dummy is a hash of a password nobody knows, for verifying against when
// there is no user to verify against.
//
// This is the whole defence against user enumeration by timing: an
// unknown email must cost the same argon2 derivation a known one does,
// and returning early instead would let anyone with a stopwatch read the
// user list out of the login endpoint. It is derived once and reused
// because computing it per attempt would itself be a signal.
var Dummy = sync.OnceValue(func() string {
	salt := make([]byte, saltLength)
	// A failure here leaves the salt zeroed, which is harmless: nothing
	// is ever verified against this hash successfully by construction.
	_, _ = rand.Read(salt)
	return encode(salt, derive(string(salt), salt, timeCost, memoryCost, parallelism, keyLength))
})

func derive(plain string, salt []byte, t, m uint32, p uint8, length uint32) []byte {
	return argon2.IDKey([]byte(plain), salt, t, m, p, length)
}

func encode(salt, digest []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryCost, timeCost, parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(digest))
}

func decode(encoded string) (t, m uint32, p uint8, salt, digest []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: not an argon2id PHC string", ErrMalformed)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: unsupported version %q", ErrMalformed, parts[2])
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: unreadable parameters %q", ErrMalformed, parts[3])
	}
	if m == 0 || t == 0 || p == 0 {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: zero parameter in %q", ErrMalformed, parts[3])
	}

	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: unreadable salt", ErrMalformed)
	}
	if digest, err = b64.DecodeString(parts[5]); err != nil || len(digest) == 0 {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: unreadable digest", ErrMalformed)
	}
	return t, m, p, salt, digest, nil
}
