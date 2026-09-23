package clitime

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"days", "30d", 30 * 24 * time.Hour, false},
		{"ninety days", "90d", 90 * 24 * time.Hour, false},
		{"one day", "1d", 24 * time.Hour, false},
		{"zero days", "0d", 0, false},

		{"hours", "24h", 24 * time.Hour, false},
		{"minutes", "90m", 90 * time.Minute, false},
		{"seconds", "30s", 30 * time.Second, false},
		{"mixed", "1h30m", 90 * time.Minute, false},

		{"nonsense", "forever", 0, true},
		{"empty", "", 0, true},
		{"bare number", "30", 0, true},
		{"negative days", "-5d", 0, true},
		{"fractional days", "1.5d", 0, true},
		{"a date", "2026-08-01", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(tt.in)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		in      string
		want    time.Time
		wantErr bool
	}{
		{"days", "30d", now.Add(-30 * 24 * time.Hour), false},
		{"a week", "7d", now.Add(-7 * 24 * time.Hour), false},
		{"hours", "24h", now.Add(-24 * time.Hour), false},
		{"minutes", "90m", now.Add(-90 * time.Minute), false},
		{"zero days", "0d", now, false},
		{"date", "2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), false},
		{"date and time", "2026-08-01 09:30", time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC), false},
		{"rfc3339", "2026-08-01T09:30:00Z", time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC), false},

		{"nonsense", "yesterday", time.Time{}, true},
		{"empty", "", time.Time{}, true},
		{"negative days", "-5d", time.Time{}, true},
		{"bare number", "30", time.Time{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWhen(tt.in, now)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && !got.Equal(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
