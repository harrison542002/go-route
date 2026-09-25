package domains

import (
	"testing"
	"time"
)

func TestClockWindow(t *testing.T) {
	at := time.Date(2026, 2, 28, 23, 59, 30, 500, time.UTC)

	tests := []struct {
		kind       WindowKind
		start, end time.Time
	}{
		{WindowMinute, time.Date(2026, 2, 28, 23, 59, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{WindowHour, time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{WindowDay, time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		// February is the month most likely to expose a fixed-length
		// month assumption.
		{WindowMonth, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			w, err := ClockWindow(tt.kind, at)
			if err != nil {
				t.Fatal(err)
			}
			if !w.Start.Equal(tt.start) || !w.End.Equal(tt.end) {
				t.Errorf("window = [%v, %v), want [%v, %v)", w.Start, w.End, tt.start, tt.end)
			}
			if w.Kind != tt.kind {
				t.Errorf("kind = %q", w.Kind)
			}
		})
	}
}

// Every replica has to land on the same counter whatever its local zone,
// or two replicas enforce two different budgets.
func TestClockWindow_IsCutInUTC(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	at := time.Date(2026, 9, 1, 2, 0, 0, 0, tokyo) // still 31 Aug in UTC

	w, err := ClockWindow(WindowDay, at)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC); !w.Start.Equal(want) {
		t.Errorf("start = %v, want %v", w.Start, want)
	}

	m, _ := ClockWindow(WindowMonth, at)
	if want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC); !m.Start.Equal(want) {
		t.Errorf("month start = %v, want %v", m.Start, want)
	}
}

func TestClockWindow_BoundaryBelongsToTheNextWindow(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	w, _ := ClockWindow(WindowMonth, at)
	if !w.Start.Equal(at) {
		t.Errorf("start = %v, want the instant itself: windows are [start, end)", w.Start)
	}
}

func TestClockWindow_RejectsPeriod(t *testing.T) {
	if _, err := ClockWindow(WindowPeriod, time.Now()); err == nil {
		t.Error("a period has no clock boundary; it must come from the quota row")
	}
}

func TestQuota_PeriodWindow(t *testing.T) {
	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	q := Quota{WindowKind: WindowPeriod, PeriodStart: &start, PeriodEnd: &end, MaxCost: USD(10)}

	tests := []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"before the period", start.Add(-time.Second), false},
		{"at the start", start, true},
		{"inside", start.Add(10 * 24 * time.Hour), true},
		{"at the end, which is exclusive", end, false},
		{"after", end.Add(time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ok := q.WindowAt(tt.at)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && (!w.Start.Equal(start) || !w.End.Equal(end) || w.Kind != WindowPeriod) {
				t.Errorf("window = %+v", w)
			}
		})
	}
}

func TestApplicableLimits_SkipsPeriodsNotInForce(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	expiredStart, expiredEnd := at.AddDate(0, -2, 0), at.AddDate(0, -1, 0)
	quotas := []Quota{
		{WindowKind: WindowMinute, MaxCost: USD(60)},
		{WindowKind: WindowPeriod, PeriodStart: &expiredStart, PeriodEnd: &expiredEnd, MaxCost: USD(1)},
		{WindowKind: WindowDay, MaxCost: USD(1_000_000_000)},
	}

	got := ApplicableLimits(quotas, at)
	if len(got) != 2 {
		t.Fatalf("got %d limits, want 2 (the expired period limits nothing)", len(got))
	}
	if got[0].Window.Kind != WindowMinute || got[1].Window.Kind != WindowDay {
		t.Errorf("kinds = %s, %s", got[0].Window.Kind, got[1].Window.Kind)
	}
	if got[0].MaxCost != USD(60) || !got[0].Block {
		t.Errorf("limit = %+v, want the row's cap and its action", got[0])
	}
}

// block is the default: a row with no action stated must not quietly
// become a soft limit.
func TestQuota_BlocksUnlessAllowed(t *testing.T) {
	if !(Quota{}).Blocks() || !(Quota{OnExceed: QuotaBlock}).Blocks() {
		t.Error("block and unset must block")
	}
	if (Quota{OnExceed: QuotaAllow}).Blocks() {
		t.Error("allow must not block")
	}
}

func TestQuotaUsage_Arithmetic(t *testing.T) {
	a := QuotaUsage{Requests: 1, Tokens: 100, Cost: 50}
	b := QuotaUsage{Requests: 1, Tokens: 40, Cost: 70}

	if got := b.Sub(a); got != (QuotaUsage{Requests: 0, Tokens: -60, Cost: 20}) {
		t.Errorf("Sub = %+v", got)
	}
	if got := a.Add(b); got != (QuotaUsage{Requests: 2, Tokens: 140, Cost: 120}) {
		t.Errorf("Add = %+v", got)
	}
	if !(QuotaUsage{}).IsZero() || a.IsZero() {
		t.Error("IsZero")
	}
}

func TestQuotaBreach_ResetsAtTheWindowEnd(t *testing.T) {
	w, _ := ClockWindow(WindowHour, time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC))
	b := QuotaBreach{Window: w, Used: FromDollars(0.9), Requested: FromDollars(0.2), Limit: FromDollars(1)}

	if want := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC); !b.ResetAt().Equal(want) {
		t.Errorf("ResetAt = %v, want %v", b.ResetAt(), want)
	}

	// The message is the whole 429 body, in dollars rather than the
	// nanodollars the cap is stored in.
	want := "spend per hour: used $0.9000 of $1.00, this request needs up to $0.2000 more; resets 2026-09-21T13:00:00Z"
	if got := b.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
