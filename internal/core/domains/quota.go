package domains

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Window is one concrete stretch of time a counter covers: [Start, End).
type Window struct {
	Kind  WindowKind
	Start time.Time
	End   time.Time
}

// ClockWindow returns the calendar window of kind containing at. Windows are
// cut in UTC.
func ClockWindow(kind WindowKind, at time.Time) (Window, error) {
	at = at.UTC()

	var start, end time.Time
	switch kind {
	case WindowMinute:
		start = at.Truncate(time.Minute)
		end = start.Add(time.Minute)
	case WindowHour:
		start = at.Truncate(time.Hour)
		end = start.Add(time.Hour)
	case WindowDay:
		start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case WindowMonth:
		start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default:
		return Window{}, fmt.Errorf("domains: %q is not a clock window", kind)
	}
	return Window{Kind: kind, Start: start, End: end}, nil
}

// WindowAt returns the window this quota applies to at the given time. It is
// false for a period that does not cover at: a billing period that has ended,
// or not yet begun, limits nothing, and the sync job that owns quotas is
// expected to replace it with the next one.
func (q Quota) WindowAt(at time.Time) (Window, bool) {
	if q.WindowKind == WindowPeriod {
		if q.PeriodStart == nil || q.PeriodEnd == nil {
			return Window{}, false
		}
		if at.Before(*q.PeriodStart) || !at.Before(*q.PeriodEnd) {
			return Window{}, false
		}
		return Window{Kind: WindowPeriod, Start: q.PeriodStart.UTC(), End: q.PeriodEnd.UTC()}, true
	}

	w, err := ClockWindow(q.WindowKind, at)
	if err != nil {
		return Window{}, false
	}
	return w, true
}

// Blocks reports whether crossing this quota rejects the request. Anything but
// an explicit allow blocks, so a row with no action stated is a hard limit.
func (q Quota) Blocks() bool {
	return q.OnExceed != QuotaAllow
}

// WindowLimit is a quota resolved against the clock: the counter it governs,
// what may be spent in it, and whether crossing it rejects the request.
type WindowLimit struct {
	Window  Window
	MaxCost USD
	Block   bool
}

// ApplicableLimits resolves every quota that covers at. The result is what one
// reservation must check, all together or not at all.
func ApplicableLimits(quotas []Quota, at time.Time) []WindowLimit {
	out := make([]WindowLimit, 0, len(quotas))
	for _, q := range quotas {
		if w, ok := q.WindowAt(at); ok {
			out = append(out, WindowLimit{Window: w, MaxCost: q.MaxCost, Block: q.Blocks()})
		}
	}
	return out
}

// QuotaUsage is what a quota counter holds, and also what one request adds to it.
type QuotaUsage struct {
	Requests int64
	Tokens   int64
	Cost     USD
}

func (u QuotaUsage) Sub(o QuotaUsage) QuotaUsage {
	return QuotaUsage{
		Requests: u.Requests - o.Requests,
		Tokens:   u.Tokens - o.Tokens,
		Cost:     u.Cost - o.Cost,
	}
}

func (u QuotaUsage) Add(o QuotaUsage) QuotaUsage {
	return QuotaUsage{
		Requests: u.Requests + o.Requests,
		Tokens:   u.Tokens + o.Tokens,
		Cost:     u.Cost + o.Cost,
	}
}

func (u QuotaUsage) IsZero() bool {
	return u == QuotaUsage{}
}

// QuotaBreach describes a spend cap a request would cross: enough for the
// caller to be told which window, how far along they are, and when it resets,
// without a second lookup.
type QuotaBreach struct {
	Window    Window
	Used      USD
	Requested USD
	Limit     USD
}

// ResetAt is when the window rolls over and the limit frees up.
func (b QuotaBreach) ResetAt() time.Time {
	return b.Window.End
}

func (b QuotaBreach) String() string {
	return fmt.Sprintf("spend per %s: used %s of %s, this request needs up to %s more; resets %s",
		b.Window.Kind, b.Used.Auto(), b.Limit.Auto(), b.Requested.Auto(),
		b.ResetAt().UTC().Format(time.RFC3339))
}

// WindowUsage is usage on one tenant's counter for one window: a change the
// flusher adds to the durable counter, or a snapshot a live counter is seeded
// from. The durable side takes changes rather than absolutes, so two replicas
// flushing the same second compose.
type WindowUsage struct {
	TenantID uuid.UUID
	Window   Window
	Usage    QuotaUsage
}
