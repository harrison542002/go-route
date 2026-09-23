// Package clitime parses the time values both command lines accept, so
// go-route and go-route-admin take the same forms.
package clitime

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration accepts what time.ParseDuration does, plus the "30d" form it
// rejects.
func ParseDuration(s string) (time.Duration, error) {
	if d, ok := parseDayDuration(s); ok {
		return d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (30d, 24h, 90m)", s)
	}
	return d, nil
}

// ParseWhen accepts a duration back from now ("30d", "24h") or an absolute
// date ("2026-08-01").
func ParseWhen(s string, now time.Time) (time.Time, error) {
	if d, err := ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"%q is neither a duration (30d, 24h) nor a date (2026-08-01)", s)
}

// parseDayDuration handles the "30d" form, which time.ParseDuration rejects.
func parseDayDuration(s string) (time.Duration, bool) {
	if !strings.HasSuffix(s, "d") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * 24 * time.Hour, true
}
