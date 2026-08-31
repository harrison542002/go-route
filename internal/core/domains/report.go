package domains

import "time"

type Report struct {
	Spec            ReportRange
	Rows            []ReportRow
	Total           ReportRow
	TruncatedGroups int64
}

type ReportRange struct {
	Tenant Tenant
	Since  time.Time
	Until  time.Time
}

type ReportRow struct {
	Key string

	Requests int64

	Cost     USD
	Unpriced int64

	Usage TokenUsage

	Comparisons map[string]Comparison

	OK           int64
	Failed       int64
	Truncated    int64
	Disconnected int64

	P50TTFTMs  int
	P95TTFTMs  int
	P95TotalMs int
}

type Comparison struct {
	Cost     USD
	Requests int64
}

// IsPartial reports whether this comparison covers fewer requests than
// the group contains. Renderings must mark partial comparisons.
func (c Comparison) IsPartial(groupRequests int64) bool {
	return c.Requests < groupRequests
}

// Savings returns actual minus what the named target would have cost,
// negative when the chosen routing was cheaper.
func (r ReportRow) Savings(vs string) (USD, bool) {
	cf, ok := r.Comparisons[vs]
	if !ok {
		return 0, false
	}
	return r.Cost - cf.Cost, true
}

// Coverage is the fraction of requests that could be priced. A report
// showing a total should show this next to it.
func (r ReportRow) Coverage() float64 {
	if r.Requests == 0 {
		return 1
	}
	return float64(r.Requests-r.Unpriced) / float64(r.Requests)
}
