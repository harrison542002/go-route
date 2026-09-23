package password

import (
	"errors"
	"strings"
	"testing"
)

const good = "correct-horse-battery-staple"

func TestHashVerifyRoundTrip(t *testing.T) {
	encoded, err := Hash(good)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, good) {
		t.Fatalf("the password is in its own hash: %q", encoded)
	}
	if err := Verify(encoded, good); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

func TestEachHashGetsItsOwnSalt(t *testing.T) {
	first, err := Hash(good)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Hash(good)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two hashes of one password are identical; the salt is not per-password")
	}
}

func TestVerifyRefusesTheWrongPassword(t *testing.T) {
	encoded, err := Hash(good)
	if err != nil {
		t.Fatal(err)
	}

	for name, attempt := range map[string]string{
		"wrong":          "incorrect-horse-battery-staple",
		"empty":          "",
		"one off":        good + "!",
		"prefix":         good[:len(good)-1],
		"case shifted":   strings.ToUpper(good),
		"with a newline": good + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(encoded, attempt); !errors.Is(err, ErrMismatch) {
				t.Errorf("Verify = %v, want ErrMismatch", err)
			}
		})
	}
}

func TestVerifyRefusesAHashItCannotRead(t *testing.T) {
	encoded, err := Hash(good)
	if err != nil {
		t.Fatal(err)
	}

	for name, stored := range map[string]string{
		"empty":            "",
		"not phc":          "not-a-hash",
		"bcrypt":           "$2a$10$abcdefghijklmnopqrstuv",
		"unknown scheme":   strings.Replace(encoded, "argon2id", "argon2i", 1),
		"future version":   strings.Replace(encoded, "v=19", "v=20", 1),
		"no parameters":    "$argon2id$v=19$$c2FsdA$ZGlnZXN0",
		"zero memory":      strings.Replace(encoded, "m=65536", "m=0", 1),
		"unreadable salt":  "$argon2id$v=19$m=65536,t=3,p=2$!!!not-base64!!!$ZGlnZXN0",
		"truncated":        encoded[:20],
		"missing digest":   encoded[:strings.LastIndex(encoded, "$")+1],
		"plaintext stored": good,
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(stored, good)
			if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrMismatch) {
				t.Errorf("Verify = %v, want a refusal", err)
			}
			if err == nil {
				t.Error("a hash that could not be read verified")
			}
		})
	}
}

func TestHashRefusesAShortPassword(t *testing.T) {
	if _, err := Hash(strings.Repeat("a", MinLength-1)); !errors.Is(err, ErrTooShort) {
		t.Errorf("Hash = %v, want ErrTooShort", err)
	}
	if _, err := Hash(strings.Repeat("a", MinLength)); err != nil {
		t.Errorf("Hash = %v", err)
	}
}

// The dummy hash has to be a real one, or verifying against it would take a
// different amount of work than verifying against a stored password -- which
// is the timing signal it exists to remove.
func TestDummyIsAWorkingHashNothingMatches(t *testing.T) {
	if err := Verify(Dummy(), good); !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify against the dummy = %v, want ErrMismatch", err)
	}
	first, second := Dummy(), Dummy()
	if first != second {
		t.Error("the dummy hash is recomputed per call")
	}
}
