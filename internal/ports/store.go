//go:generate mockgen -source=store.go -destination=mocks/store_mock.go -package=mocks

package ports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var (
	ErrDecisionNotFound = errors.New("store: decision not found")
	ErrUnknownTenant    = errors.New("store: unknown tenant")
)

// DecisionRepository reads the decision log.
type DecisionRepository interface {
	// Get retrieves one decision in full, for `go-route explain`.
	Get(ctx context.Context, id domains.DecisionID) (domains.RoutingDecision, error)

	// Aggregate answers report questions.
	Aggregate(ctx context.Context, spec ReportSpec) (domains.Report, error)
}

// ReportSpec constrains what a report may ask.
type ReportSpec struct {
	Tenant domains.Tenant
	Since  time.Time
	Until  time.Time

	GroupBy GroupBy

	// MetaKey names the metadata field to group by, when GroupBy is
	// GroupByMetadata. Metadata keys vary per deployment, so this cannot
	// be an enum.
	MetaKey string

	// Limit caps the number of groups returned. Zero uses
	// DefaultReportLimit. A high-cardinality metadata key would
	// otherwise return one row per request.
	Limit int
}

const (
	DefaultReportLimit = 500

	MaxReportLimit = 5000
)

func (s ReportSpec) Validate() error {
	if s.Since.IsZero() || s.Until.IsZero() {
		return fmt.Errorf("report: since and until are required")
	}
	if !s.Since.Before(s.Until) {
		return fmt.Errorf("report: since (%s) must be before until (%s)",
			s.Since.Format(time.RFC3339), s.Until.Format(time.RFC3339))
	}
	if s.GroupBy == GroupByMetadata && s.MetaKey == "" {
		return fmt.Errorf("report: grouping by metadata needs a key")
	}
	if s.Limit < 0 {
		return fmt.Errorf("report: limit must not be negative")
	}
	if s.Limit > MaxReportLimit {
		return fmt.Errorf("report: limit %d is above the maximum of %d", s.Limit, MaxReportLimit)
	}
	// The empty value is the ungrouped report; the SQL's CASE resolves any other
	// unknown grouping to it, answering a misspelled group_by with a bare total.
	if s.GroupBy != "" && !s.GroupBy.Valid() {
		return fmt.Errorf("report: unknown grouping %q", s.GroupBy)
	}
	return nil
}

type GroupBy string

const (
	GroupByNone     GroupBy = "none"
	GroupByModel    GroupBy = "model"  // the alias clients requested
	GroupByTarget   GroupBy = "target" // the target that served
	GroupByStatus   GroupBy = "status"
	GroupByMetadata GroupBy = "metadata"
	GroupByDay      GroupBy = "day"
)

// GroupBys lists every grouping.
var GroupBys = []GroupBy{
	GroupByNone, GroupByModel, GroupByTarget, GroupByStatus, GroupByMetadata, GroupByDay,
}

func (g GroupBy) Valid() bool {
	for _, known := range GroupBys {
		if g == known {
			return true
		}
	}
	return false
}

// ObservabilityRepository is the read side the admin API's dashboard endpoints
// speak to.
type ObservabilityRepository interface {
	DecisionRepository

	// ResolveTenant maps a UUID or external id to the external id the ledger and
	// audit log are read by, returning ErrUnknownTenant when there is no such
	// tenant -- an empty report would read as no spend.
	ResolveTenant(ctx context.Context, ref string) (domains.Tenant, error)

	// ListAudit returns one page of a tenant's audit events, newest first.
	ListAudit(ctx context.Context, q AuditQuery) (AuditPage, error)
}

// AuditQuery asks for one page of a tenant's audit events.
type AuditQuery struct {
	Tenant domains.Tenant
	Since  time.Time
	Until  time.Time

	// After is where the previous page stopped. Nil starts at the newest event
	// in the range.
	After *AuditCursor

	// Limit caps the page. Zero uses DefaultAuditLimit.
	Limit int
}

// AuditCursor is the position of the last event a page returned. The pair is
// (At, ID) because ts alone is not unique: a batch flush writes several rows
// with timestamps that can collide, and a cursor that could not tell them apart
// would repeat or drop the ones sharing a microsecond.
type AuditCursor struct {
	At time.Time
	ID uuid.UUID
}

// AuditPage is one page of events and where to continue from.
type AuditPage struct {
	Events []domains.AuditEvent

	// Next is nil on the last page, including the page that filled the limit
	// exactly, so a caller never makes one more empty request.
	Next *AuditCursor
}

const (
	DefaultAuditLimit = 50
	MaxAuditLimit     = 500
)

func (q AuditQuery) Validate() error {
	if q.Tenant == "" {
		return fmt.Errorf("audit: a tenant is required")
	}
	if q.Since.IsZero() || q.Until.IsZero() {
		return fmt.Errorf("audit: since and until are required")
	}
	if !q.Since.Before(q.Until) {
		return fmt.Errorf("audit: since (%s) must be before until (%s)",
			q.Since.Format(time.RFC3339), q.Until.Format(time.RFC3339))
	}
	if q.Limit < 0 {
		return fmt.Errorf("audit: limit must not be negative")
	}
	if q.Limit > MaxAuditLimit {
		return fmt.Errorf("audit: limit %d is above the maximum of %d", q.Limit, MaxAuditLimit)
	}
	return nil
}
