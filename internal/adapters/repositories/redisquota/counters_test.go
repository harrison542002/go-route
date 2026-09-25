package redisquota

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var testNow = time.Date(2026, 9, 21, 12, 30, 0, 0, time.UTC)

func limits(kinds ...domains.WindowKind) []domains.WindowLimit {
	out := make([]domains.WindowLimit, 0, len(kinds))
	for _, k := range kinds {
		w, err := domains.ClockWindow(k, testNow)
		if err != nil {
			panic(err)
		}
		out = append(out, domains.WindowLimit{Window: w})
	}
	return out
}

// The key is shared by every replica and outlives any one of them, so
// its format is a contract. The braces are a Redis Cluster hash tag that
// keeps every window of a tenant in one slot, which one script needs.
func TestKey(t *testing.T) {
	id := uuid.MustParse("0192f7a0-1111-7222-8333-444455556666")
	w, _ := domains.ClockWindow(domains.WindowDay, testNow)

	// 1789948800 is 2026-09-21T00:00:00Z.
	want := "q:{0192f7a0-1111-7222-8333-444455556666}:day:1789948800"
	if got := Key(id, w); got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

func TestExpireAtOutlivesTheWindow(t *testing.T) {
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	if got := expireAt(w); got != w.End.Add(expiryGrace).Unix() || got <= w.End.Unix() {
		t.Errorf("expireAt = %d; a long stream must still be able to reconcile", got)
	}
}

func TestParseReserve(t *testing.T) {
	ls := limits(domains.WindowMinute, domains.WindowDay)
	amount := domains.QuotaUsage{Requests: 1, Tokens: 300, Cost: 42}

	t.Run("missing", func(t *testing.T) {
		got, err := parseReserve([]any{"missing", int64(2)}, ls, amount)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Missing) != 1 || got.Missing[0] != ls[1].Window || got.Blocked != nil {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		got, err := parseReserve([]any{"blocked", int64(1), int64(900), int64(1000)}, ls, amount)
		if err != nil {
			t.Fatal(err)
		}
		want := domains.QuotaBreach{
			Window: ls[0].Window, Used: 900, Requested: 42, Limit: 1000,
		}
		if got.Blocked == nil || *got.Blocked != want {
			t.Errorf("blocked = %+v, want %+v", got.Blocked, want)
		}
	})

	t.Run("ok with soft breaches", func(t *testing.T) {
		got, err := parseReserve([]any{"ok", int64(2), int64(10), int64(20)}, ls, amount)
		if err != nil {
			t.Fatal(err)
		}
		if got.Blocked != nil || len(got.Missing) != 0 || len(got.Soft) != 1 {
			t.Fatalf("got %+v", got)
		}
		if s := got.Soft[0]; s.Window != ls[1].Window || s.Used != 10 || s.Limit != 20 || s.Requested != 42 {
			t.Errorf("soft = %+v", s)
		}
	})

	t.Run("ok", func(t *testing.T) {
		got, err := parseReserve([]any{"ok"}, ls, amount)
		if err != nil || got.Blocked != nil || len(got.Soft) != 0 || len(got.Missing) != 0 {
			t.Errorf("got %+v, %v", got, err)
		}
	})

	for name, reply := range map[string][]any{
		"empty":                  {},
		"unknown status":         {"maybe"},
		"index out of range":     {"missing", int64(3)},
		"index is zero":          {"missing", int64(0)},
		"blocked without detail": {"blocked", int64(1)},
		"ragged soft list":       {"ok", int64(1), int64(10)},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := parseReserve(reply, ls, amount); !errors.Is(err, ErrUnexpectedReply) {
				t.Errorf("err = %v, want ErrUnexpectedReply", err)
			}
		})
	}
}
