package password

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLength is the longest password this package will hash.
//
// The cap is a denial-of-service bound, not a style rule: argon2id reads
// the whole password, so an unauthenticated caller who may post a
// megabyte of it buys 64 MiB of memory and a derivation per request at no
// cost to themselves. Because nothing may exceed it, the login path can
// refuse an over-long password before hashing without that refusal
// telling the caller anything a wrong password would not.
const MaxLength = 128

// minLocalPart is the shortest email local part worth looking for inside a
// password. Below it the rule stops being about the address -- "jo" would
// fall out of ordinary words -- and starts refusing good passwords.
const minLocalPart = 3

// productNames are the spellings of this product a password may not
// contain, lowercased.
var productNames = []string{"go-route", "goroute"}

// PolicyError names the rule a password broke. The rule is a phrase, not a
// sentence, so a caller can put it behind its own subject; the password
// itself never appears, because these travel into API responses and logs.
type PolicyError struct{ Rule string }

func (e PolicyError) Error() string { return "password: " + e.Rule }

var (
	ErrTooShort        = PolicyError{Rule: fmt.Sprintf("must be at least %d characters", MinLength)}
	ErrTooLong         = PolicyError{Rule: fmt.Sprintf("must be at most %d characters", MaxLength)}
	ErrAllWhitespace   = PolicyError{Rule: "must not be entirely whitespace"}
	ErrContainsEmail   = PolicyError{Rule: "must not contain the address it belongs to"}
	ErrContainsProduct = PolicyError{Rule: "must not contain the name of this product"}
)

// Check applies the password policy. email is the address the password
// belongs to and may be empty when there is none.
//
// There are no character-class rules here on purpose. Requiring a digit,
// a capital and a symbol moves people onto a few predictable shapes --
// Password1!, Summer2026! -- which a cracker tries first, so the rule
// costs usability and buys almost no entropy; NIST SP 800-63B tells
// verifiers not to impose them. Length and the two strings an attacker
// would guess first are what is left.
//
// Nothing is trimmed. A leading or trailing space is a character the
// person typed, and quietly dropping it would store a password that is
// not the one they wrote down.
func Check(plain, email string) error {
	if err := checkLength(plain); err != nil {
		return err
	}
	if strings.TrimSpace(plain) == "" {
		return ErrAllWhitespace
	}

	lower := strings.ToLower(plain)
	for _, name := range productNames {
		if strings.Contains(lower, name) {
			return ErrContainsProduct
		}
	}
	if local := localPart(email); utf8.RuneCountInString(local) >= minLocalPart &&
		strings.Contains(lower, local) {
		return ErrContainsEmail
	}
	return nil
}

// TooLong reports whether plain is over the cap. The login path calls this
// before it looks anything up, so an over-long password costs a rune count
// rather than an argon2 derivation.
func TooLong(plain string) bool {
	return utf8.RuneCountInString(plain) > MaxLength
}

func checkLength(plain string) error {
	switch n := utf8.RuneCountInString(plain); {
	case n < MinLength:
		return ErrTooShort
	case n > MaxLength:
		return ErrTooLong
	default:
		return nil
	}
}

func localPart(email string) string {
	if at := strings.IndexByte(email, '@'); at >= 0 {
		email = email[:at]
	}
	return strings.ToLower(strings.TrimSpace(email))
}
