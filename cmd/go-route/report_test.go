package main

import (
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/ports"
)

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
			got, err := parseWhen(tt.in, now)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && !got.Equal(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A grouping that is not a known keyword is treated as a metadata key,
// since keys vary per deployment and cannot be an enum.
func TestBuildSpec_GroupBy(t *testing.T) {
	tests := []struct {
		in      string
		want    ports.GroupBy
		wantKey string
	}{
		{"", ports.GroupByNone, ""},
		{"model", ports.GroupByModel, ""},
		{"target", ports.GroupByTarget, ""},
		{"status", ports.GroupByStatus, ""},
		{"day", ports.GroupByDay, ""},
		{"feature", ports.GroupByMetadata, "feature"},
		{"cost_center", ports.GroupByMetadata, "cost_center"},
	}

	for _, tt := range tests {
		name := tt.in
		if name == "" {
			name = "(none)"
		}
		t.Run(name, func(t *testing.T) {
			spec, err := buildSpec("7d", "", tt.in, "default", 100)
			if err != nil {
				t.Fatal(err)
			}
			if spec.GroupBy != tt.want {
				t.Errorf("GroupBy = %q, want %q", spec.GroupBy, tt.want)
			}
			if spec.MetaKey != tt.wantKey {
				t.Errorf("MetaKey = %q, want %q", spec.MetaKey, tt.wantKey)
			}
		})
	}
}

func TestBuildSpec_Defaults(t *testing.T) {
	before := time.Now()
	spec, err := buildSpec("7d", "", "", "default", 100)
	if err != nil {
		t.Fatal(err)
	}

	// Until defaults to now, so today's traffic is included. Defaulting
	// to midnight would silently exclude it.
	if spec.Until.Before(before) {
		t.Errorf("Until = %v, want now or later", spec.Until)
	}
	if !spec.Since.Before(spec.Until) {
		t.Error("Since is not before Until")
	}
}

func TestBuildSpec_Rejects(t *testing.T) {
	tests := []struct {
		name                     string
		since, until, group, ten string
		limit                    int
	}{
		{"reversed range", "2026-08-01", "2026-07-01", "", "default", 100},
		{"bad since", "nonsense", "", "", "default", 100},
		{"bad until", "7d", "nonsense", "", "default", 100},
		{"negative limit", "7d", "", "", "default", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildSpec(tt.since, tt.until, tt.group, tt.ten, tt.limit); err == nil {
				t.Error("expected an error; a bad spec must not silently produce an empty report")
			}
		})
	}
}
