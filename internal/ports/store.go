package ports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var ErrDecisionNotFound = errors.New("store: decision not found")

// DecisionStore reads the decision log.
type DecisionStore interface {
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

const DefaultReportLimit = 500

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
