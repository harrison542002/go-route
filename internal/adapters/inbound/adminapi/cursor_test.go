package adminapi

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/ports"
)

func TestCursorRoundTrips(t *testing.T) {
	for name, c := range map[string]ports.AuditCursor{
		"whole second": {At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), ID: uuid.New()},
		"microseconds": {At: time.Date(2026, 9, 1, 12, 0, 0, 123456000, time.UTC), ID: uuid.New()},
		"nanoseconds":  {At: time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC), ID: uuid.New()},
		"epoch":        {At: time.Unix(0, 0).UTC(), ID: uuid.Nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeCursor(encodeCursor(c))
			if err != nil {
				t.Fatal(err)
			}
			if !got.At.Equal(c.At) || got.ID != c.ID {
				t.Errorf("round trip = %+v, want %+v", got, c)
			}
		})
	}
}

func TestCursorIsURLSafe(t *testing.T) {
	s := encodeCursor(ports.AuditCursor{At: time.Now(), ID: uuid.New()})
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			t.Fatalf("cursor %q contains %q, which a URL would escape", s, r)
		}
	}
}

func TestCursorRejectsGarbage(t *testing.T) {
	for name, s := range map[string]string{
		"empty":              "",
		"not base64":         "not a cursor!!",
		"base64 of nonsense": base64.RawURLEncoding.EncodeToString([]byte("hello")),
		"too few fields":     base64.RawURLEncoding.EncodeToString([]byte("1." + uuid.New().String())),
		"too many fields":    base64.RawURLEncoding.EncodeToString([]byte("1.0." + uuid.New().String() + ".x")),
		"unknown version":    base64.RawURLEncoding.EncodeToString([]byte("2.0." + uuid.New().String())),
		"timestamp is words": base64.RawURLEncoding.EncodeToString([]byte("1.yesterday." + uuid.New().String())),
		"id is not a uuid":   base64.RawURLEncoding.EncodeToString([]byte("1.0.nope")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(s); err == nil {
				t.Errorf("%q decoded", s)
			}
		})
	}
}
