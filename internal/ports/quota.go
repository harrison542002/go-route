//go:generate mockgen -source=quota.go -destination=mocks/quota_mock.go -package=mocks

package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var (
	ErrQuotaExceeded    = errors.New("quota: limit exceeded")
	ErrQuotaUnavailable = errors.New("quota: enforcement unavailable")
)

// QuotaExceededError names the limit a request would have crossed.
type QuotaExceededError struct {
	Breach domains.QuotaBreach
}

func (e *QuotaExceededError) Error() string {
	return "quota: limit exceeded: " + e.Breach.String()
}

func (e *QuotaExceededError) Unwrap() error { return ErrQuotaExceeded }

// QuotaRequest is what a reservation is sized from.
type QuotaRequest struct {
	Tenant domains.Tenant
	At     time.Time

	// Prompt, MaxOutputTokens and Choices are the request's own figures; the
	// enforcer turns them into a worst case, because the default ceiling for a
	// request that sets none is quota configuration.
	Prompt          domains.PromptSize
	MaxOutputTokens int
	Choices         int

	// Targets are the ladder's target names. Which one will serve is not known
	// before dispatch, so the estimate is priced at the dearest.
	Targets []string
}

// Reservation is what Reserve took from a tenant's counters. It has to be
// handed back to Reconcile whatever happens to the request: a reservation
// never reconciled stays on the counter until the window ends, and enough of
// them lock a tenant out.
//
// The zero value is a reservation of nothing, returned when a tenant has no
// quotas or when enforcement failed open; reconciling it is a no-op.
type Reservation struct {
	TenantID uuid.UUID
	At       time.Time

	// Windows are the counters the reservation was added to. Reconcile adjusts
	// these and not whatever windows are current by then, or a stream that
	// outlives its minute would correct the wrong minute.
	Windows []domains.Window

	Reserved domains.QuotaUsage
}

func (r Reservation) IsZero() bool { return len(r.Windows) == 0 }

// QuotaEnforcer is the request path's view of quotas.
type QuotaEnforcer interface {
	// Reserve checks every quota the tenant carries and, only if none would be
	// crossed, reserves the request's worst case against all of them. It
	// returns a *QuotaExceededError when a blocking limit would be crossed and
	// ErrQuotaUnavailable when it cannot tell and the deployment fails closed.
	Reserve(ctx context.Context, req QuotaRequest) (Reservation, error)

	// Reconcile replaces the reservation with what the request actually used.
	// It never fails the request; a failure is logged. It must run even when
	// ctx is already cancelled, so implementations detach from its cancellation.
	Reconcile(ctx context.Context, res Reservation, outcome domains.Outcome)
}

// UsageCounterRepository is the durable snapshot behind the live counters: what
// a Redis that lost a window is rebuilt from.
type UsageCounterRepository interface {
	// Get returns the durable counter for one window, zero when none has been
	// written.
	Get(ctx context.Context, tenantID uuid.UUID, w domains.Window) (domains.QuotaUsage, error)

	// AddDeltas adds each delta to its durable counter.
	AddDeltas(ctx context.Context, deltas []domains.WindowUsage) error
}

// CounterReservation is the answer to one atomic check-and-reserve. Exactly one
// of Missing and Blocked is set when nothing was reserved.
type CounterReservation struct {
	// Missing lists windows with no live counter yet. Nothing was reserved;
	// the caller seeds them and tries again.
	Missing []domains.Window

	// Blocked is the first blocking limit that would be crossed. Nothing was
	// reserved.
	Blocked *domains.QuotaBreach

	// Soft lists allow limits the reservation crossed. It was reserved anyway;
	// these are for the log.
	Soft []domains.QuotaBreach
}

// QuotaCounters holds the live counters every replica enforces against. All of
// a reservation's windows are checked and incremented as one atomic step, which
// is what keeps concurrent replicas from each admitting the last request under
// the limit.
type QuotaCounters interface {
	Reserve(ctx context.Context, tenantID uuid.UUID, limits []domains.WindowLimit, amount domains.QuotaUsage) (CounterReservation, error)

	// Seed raises each counter to the durable snapshot, per field, and never
	// lowers one. Taking a maximum rather than adding means replicas seeding
	// the same window at once cannot count its history twice, and a counter
	// that came back from a failover holding stale values is corrected.
	Seed(ctx context.Context, seeds []domains.WindowUsage) error

	// Adjust adds delta to counters that still exist. One that has expired is
	// not recreated: its window is over.
	Adjust(ctx context.Context, tenantID uuid.UUID, windows []domains.Window, delta domains.QuotaUsage) error
}
