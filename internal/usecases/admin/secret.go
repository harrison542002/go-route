package admin

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"io"
)

const (
	secretEntropyBytes = 32

	// displayChars is how much of the random part a stored prefix keeps: enough
	// to tell credentials apart, nowhere near enough to narrow a guess.
	displayChars = 6
)

// secretEncoding is lowercase base32 without padding: no characters that need
// escaping in a header, a URL or a shell, and none that read alike.
var secretEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Minted is one freshly generated secret: the plaintext, handed over exactly
// once, the hash stored in its place, and the visible head for display.
type Minted struct {
	Secret string
	Prefix string
	Hash   [sha256.Size]byte
}

// Mint generates a credential secret under the given prefix. Tenant API keys
// and admin credentials are both minted here: one place to get the entropy,
// the encoding and the hashing right.
func Mint(prefix string, random io.Reader) (Minted, error) {
	b := make([]byte, secretEntropyBytes)
	if _, err := io.ReadFull(random, b); err != nil {
		return Minted{}, fmt.Errorf("admin: generate secret: %w", err)
	}

	secret := prefix + secretEncoding.EncodeToString(b)
	return Minted{
		Secret: secret,
		Prefix: secret[:len(prefix)+displayChars],
		Hash:   sha256.Sum256([]byte(secret)),
	}, nil
}

// hashSecret is how a presented secret is looked up: the plaintext never
// travels to the database, nor into a query log.
func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}
