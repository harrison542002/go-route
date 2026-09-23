package adminapi

import (
	"sort"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// Where the observability domain types meet the wire ones. Costs cross as
// nanodollars and are named so: rendering dollars here would round every figure
// before a caller could add two of them together.

func toUsageReport(spec ports.ReportSpec, r domains.Report) gen.UsageReport {
	out := gen.UsageReport{
		Spec: gen.UsageSpec{
			Tenant:  string(r.Spec.Tenant),
			Since:   r.Spec.Since,
			Until:   r.Spec.Until,
			GroupBy: gen.UsageGroupBy(spec.GroupBy),
		},
		Rows:            make([]gen.UsageRow, 0, len(r.Rows)),
		Total:           toUsageTotals(r.Total),
		TruncatedGroups: r.TruncatedGroups,
	}
	if spec.GroupBy == ports.GroupByMetadata {
		key := spec.MetaKey
		out.Spec.MetaKey = &key
	}
	for _, row := range r.Rows {
		out.Rows = append(out.Rows, toUsageRow(row))
	}
	return out
}

// toUsageRow renders a group. Latency belongs here and not on the total: a p95
// of p95s is not a p95.
func toUsageRow(r domains.ReportRow) gen.UsageRow {
	t := toUsageTotals(r)
	return gen.UsageRow{
		Key:          r.Key,
		Requests:     t.Requests,
		CostNanos:    t.CostNanos,
		Unpriced:     t.Unpriced,
		Tokens:       t.Tokens,
		Comparisons:  t.Comparisons,
		Ok:           t.Ok,
		Failed:       t.Failed,
		Truncated:    t.Truncated,
		Disconnected: t.Disconnected,
		P50TtftMs:    r.P50TTFTMs,
		P95TtftMs:    r.P95TTFTMs,
		P95TotalMs:   r.P95TotalMs,
	}
}

func toUsageTotals(r domains.ReportRow) gen.UsageTotals {
	return gen.UsageTotals{
		Requests:     r.Requests,
		CostNanos:    int64(r.Cost),
		Unpriced:     r.Unpriced,
		Tokens:       toTokenCounts(r.Usage),
		Comparisons:  toComparisons(r.Comparisons),
		Ok:           r.OK,
		Failed:       r.Failed,
		Truncated:    r.Truncated,
		Disconnected: r.Disconnected,
	}
}

// toComparisons flattens the domain's map into an array carrying the target it
// compares against. The order must stay sorted: a response that shuffles its
// own array between identical requests makes a dashboard's rows jump.
func toComparisons(m map[string]domains.Comparison) []gen.UsageComparison {
	out := make([]gen.UsageComparison, 0, len(m))
	for target, c := range m {
		out = append(out, gen.UsageComparison{
			Target:    target,
			CostNanos: int64(c.Cost),
			Requests:  c.Requests,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

func toTokenCounts(u domains.TokenUsage) gen.TokenCounts {
	return gen.TokenCounts{
		Input:      int64(u.Input),
		Output:     int64(u.Output),
		CacheRead:  int64(u.CacheRead),
		CacheWrite: int64(u.CacheWrite),
		Reasoning:  int64(u.Reasoning),
	}
}

func toAuditPage(p ports.AuditPage) gen.AuditPage {
	out := gen.AuditPage{
		Events:     make([]gen.AuditEvent, 0, len(p.Events)),
		NextCursor: nullable.NewNullNullable[string](),
	}
	for _, e := range p.Events {
		out.Events = append(out.Events, toAuditEvent(e))
	}
	if p.Next != nil {
		out.NextCursor = nullable.NewNullableWithValue(encodeCursor(*p.Next))
	}
	return out
}

func toAuditEvent(e domains.AuditEvent) gen.AuditEvent {
	return gen.AuditEvent{
		Id:          e.ID,
		At:          e.At,
		TenantId:    nullableOf(e.TenantID),
		KeyId:       nullableOf(e.KeyID),
		Actor:       e.Actor,
		Action:      e.Action,
		RequestId:   nullableString(e.RequestID),
		Detail:      nullableString(e.Detail),
		FinalTarget: nullableString(e.FinalTarget),
		StatusCode:  nullableOf(e.StatusCode),
		Error:       nullableString(e.Error),
	}
}

// nullableString renders an empty column as an explicit JSON null; "" would
// claim a value the row does not have.
func nullableString(s string) nullable.Nullable[string] {
	if s == "" {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(s)
}

func toDecision(d domains.RoutingDecision) gen.Decision {
	out := gen.Decision{
		Id:         d.ID.String(),
		OccurredAt: d.OccurredAt,
		Tenant:     string(d.Tenant),
		KeyId:      nullable.NewNullNullable[uuid.UUID](),
		Request: gen.DecisionRequest{
			RequestedModel: d.Request.RequestedModel,
			Stream:         d.Request.Stream,
			WantsUsage:     d.Request.WantsUsage,
			Metadata:       d.Request.Metadata,
		},
		Ladder:   toLadder(d.Ladder),
		Status:   gen.DecisionStatus(d.Outcome.Status),
		Attempts: toAttempts(d.Outcome.Attempts),
		Tokens:   toTokenCounts(d.Outcome.Usage),
		Timing:   gen.DecisionTiming{TtftMs: d.Outcome.TTFTMs, TotalMs: d.Outcome.TotalMs},
	}
	if d.KeyID != uuid.Nil {
		out.KeyId = nullable.NewNullableWithValue(d.KeyID)
	}
	if d.Request.Metadata == nil {
		out.Request.Metadata = map[string]string{}
	}
	if chosen := d.Outcome.ChosenTarget(); chosen != "" {
		out.ChosenTarget = &chosen
	}
	// A never-priced request is a different fact from one costing zero, so the
	// field is absent rather than zero.
	if d.Cost != nil {
		out.Cost = toDecisionCost(*d.Cost)
	}
	return out
}

func toLadder(l domains.Ladder) gen.DecisionLadder {
	out := gen.DecisionLadder{Targets: make([]gen.DecisionTarget, 0, len(l.Targets))}
	for _, t := range l.Targets {
		target := gen.DecisionTarget{Name: t.Name}
		target.Provider = optionalString(t.Provider)
		target.UpstreamModel = optionalString(t.UpstreamModel)
		target.Region = optionalString(t.Region)
		out.Targets = append(out.Targets, target)
	}
	if l.Reason.Kind != "" {
		kind := string(l.Reason.Kind)
		out.ReasonKind = &kind
	}
	out.ModelAlias = optionalString(l.Reason.ModelAlias)
	out.RuleName = optionalString(l.Reason.RuleName)
	if l.Reason.PolicyVersion != 0 {
		v := l.Reason.PolicyVersion
		out.PolicyVersion = &v
	}
	return out
}

func toAttempts(attempts []domains.Attempt) []gen.DecisionAttempt {
	out := make([]gen.DecisionAttempt, 0, len(attempts))
	for _, a := range attempts {
		attempt := gen.DecisionAttempt{
			Target:     a.Target,
			StartedAt:  a.StartedAt,
			DurationMs: a.DurationMs,
		}
		if a.Failure != nil {
			attempt.Failure = &gen.DecisionAttemptFailure{
				Kind:      a.Failure.Kind,
				Message:   a.Failure.Message,
				Retryable: a.Failure.Retryable,
			}
			if a.Failure.StatusCode != 0 {
				code := a.Failure.StatusCode
				attempt.Failure.StatusCode = &code
			}
		}
		out = append(out, attempt)
	}
	return out
}

func toDecisionCost(c domains.CostBreakdown) *gen.DecisionCost {
	out := &gen.DecisionCost{ActualNanos: int64(c.Actual)}
	out.PriceTableVersion = optionalString(c.PriceTableVersion)

	cfs := make([]gen.DecisionCounterfactual, 0, len(c.Counterfactuals))
	for _, cf := range c.Counterfactuals {
		cfs = append(cfs, gen.DecisionCounterfactual{Target: cf.Target, CostNanos: int64(cf.Cost)})
	}
	out.Counterfactuals = &cfs
	return out
}

// optionalString omits an empty string rather than rendering "", which would be
// a value.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
