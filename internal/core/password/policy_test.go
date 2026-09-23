package password

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

const policyEmail = "ops.team@example.com"

func TestCheckRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		plain string
		want  error
	}{
		"empty":               {"", ErrTooShort},
		"eleven ascii":        {strings.Repeat("a", MinLength-1), ErrTooShort},
		"eleven multibyte":    {strings.Repeat("パ", MinLength-1), ErrTooShort},
		"one over the cap":    {strings.Repeat("a", MaxLength+1), ErrTooLong},
		"far over the cap":    {strings.Repeat("a", 100_000), ErrTooLong},
		"multibyte over cap":  {strings.Repeat("パ", MaxLength+1), ErrTooLong},
		"all spaces":          {strings.Repeat(" ", MinLength+4), ErrAllWhitespace},
		"tabs and newlines":   {strings.Repeat("\t\n", MinLength), ErrAllWhitespace},
		"product name":        {"my-go-route-admin-password", ErrContainsProduct},
		"product name run in": {"xyzzyGoRouteSecret42", ErrContainsProduct},
		"local part":          {"ops.team-is-my-password", ErrContainsEmail},
		"local part shouting": {"zzzOPS.TEAMzzzzzzzzzz", ErrContainsEmail},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check(tc.plain, policyEmail)
			if !errors.Is(err, tc.want) {
				t.Errorf("Check = %v, want %v", err, tc.want)
			}
			if tc.plain != "" && strings.Contains(err.Error(), tc.plain) {
				t.Errorf("the message quotes the password: %q", err)
			}
		})
	}
}

// The rune count is the point: a byte-length check would let an
// eleven-character passphrase through in any script but Latin.
func TestElevenMultibyteRunesAreElevenCharacters(t *testing.T) {
	short := strings.Repeat("パ", MinLength-1)
	if len(short) < MinLength {
		t.Fatalf("this case proves nothing: %q is %d bytes", short, len(short))
	}
	if utf8.RuneCountInString(short) != MinLength-1 {
		t.Fatalf("rune count = %d", utf8.RuneCountInString(short))
	}
}

func TestCheckAccepts(t *testing.T) {
	for name, plain := range map[string]string{
		"exactly the minimum":  "twelve chars",
		"exactly the cap":      strings.Repeat("a long passphrase ", 8)[:MaxLength],
		"a generated secret":   "qv7m2kx9dphs4btn6rwz3yjc8aefl5gu2knxm3btq6vhs2z",
		"a passphrase":         "correct horse battery staple",
		"leading space kept":   "  a quiet passphrase",
		"trailing space kept":  "a quiet passphrase  ",
		"twelve runes of kana": strings.Repeat("パ", MinLength),
		"a japanese sentence":  "私のパスワードはとても長いです",
		"cyrillic":             "правильная лошадь батарейка",
		"emoji":                "🔐 a locked door and a long phrase",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Check(plain, policyEmail); err != nil {
				t.Errorf("Check = %v", err)
			}
		})
	}
}

// A local part below minLocalPart is not looked for: "jo" falls out of
// ordinary words, and refusing every password containing it would refuse
// good ones for nothing.
func TestShortLocalPartsAreNotLookedFor(t *testing.T) {
	if err := Check("jolly-good-passphrase", "jo@example.com"); err != nil {
		t.Errorf("Check = %v", err)
	}
	if err := Check("joe-is-my-passphrase", "joe@example.com"); !errors.Is(err, ErrContainsEmail) {
		t.Errorf("Check = %v, want ErrContainsEmail", err)
	}
}

func TestCheckWithoutAnEmail(t *testing.T) {
	if err := Check("a perfectly fine passphrase", ""); err != nil {
		t.Errorf("Check = %v", err)
	}
}

func TestTooLongMatchesTheCap(t *testing.T) {
	if TooLong(strings.Repeat("a", MaxLength)) {
		t.Error("a password at the cap is reported as too long")
	}
	if !TooLong(strings.Repeat("a", MaxLength+1)) {
		t.Error("a password over the cap is not reported as too long")
	}
	if !TooLong(strings.Repeat("パ", MaxLength+1)) {
		t.Error("the cap is counting bytes, not runes")
	}
}

func TestHashRefusesAnOverLongPassword(t *testing.T) {
	if _, err := Hash(strings.Repeat("a", MaxLength+1)); !errors.Is(err, ErrTooLong) {
		t.Errorf("Hash = %v, want ErrTooLong", err)
	}
	if _, err := Hash(strings.Repeat("a", MaxLength)); err != nil {
		t.Errorf("Hash = %v", err)
	}
}
