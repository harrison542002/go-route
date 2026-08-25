package domains

import "time"

// RoutingDecision is the audit record for one request: what was asked,
// what was decided, why, what happened, and what it cost.
type RoutingDecision struct {
	ID         DecisionID
	OccurredAt time.Time
	Tenant     Tenant

	// Request is what the client asked for, without the body.
	Request RequestSummary

	// Ladder is the decision: which targets were eligible, in what order,
	// and why.
	Ladder Ladder

	// Outcome is what actually happened, including every attempt.
	Outcome Outcome

	// Cost is nil until pricing lands. A nil cost means "not computed",
	// which is distinct from a computed zero.
	Cost *CostBreakdown
}

// RequestSummary is the non-sensitive shape of the request.
type RequestSummary struct {
	RequestedModel string
	Stream         bool
	WantsUsage     bool
	Metadata       map[string]string
}

// CostBreakdown is reserved for the pricing layer. PriceTableRef records
// which dated price table was applied, so a provider changing prices does
// not silently rewrite the cost of past traffic.
type CostBreakdown struct {
	Actual        USD
	PriceTableRef string
	// Counterfactuals []Counterfactual — added with the pricing layer.
}

func NewRoutingDecision(
	id DecisionID,
	facts RequestFacts,
	ladder Ladder,
	outcome Outcome,
) RoutingDecision {
	return RoutingDecision{
		ID:         id,
		OccurredAt: facts.ReceivedAt,
		Tenant:     facts.Tenant,
		Request: RequestSummary{
			RequestedModel: facts.RequestedModel,
			Stream:         facts.Stream,
			WantsUsage:     facts.WantsUsage,
			Metadata:       facts.Metadata,
		},
		Ladder:  ladder,
		Outcome: outcome,
	}
}
