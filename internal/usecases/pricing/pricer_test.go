package pricing

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var (
	augustFifteenth = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	beforeAnyPrices = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

type stubTable struct {
	rates map[string]domains.Rates
	from  time.Time
}

func (s stubTable) RatesAt(at time.Time, target string) (domains.Rates, string, error) {
	if at.Before(s.from) {
		return domains.Rates{}, "", ports.ErrNoPricing
	}
	r, ok := s.rates[target]
	if !ok {
		return domains.Rates{}, "", ports.ErrNoPricing
	}
	return r, "2026-08-01", nil
}

func rates(in, out float64) domains.Rates {
	return domains.Rates{
		Input:  domains.PerMillionTokens(domains.FromDollars(in)),
		Output: domains.PerMillionTokens(domains.FromDollars(out)),
	}
}

func table() stubTable {
	return stubTable{
		from: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		rates: map[string]domains.Rates{
			"openai/gpt-5-mini": rates(0.25, 2.00),
			"openai/gpt-5":      rates(1.25, 10.00),
			"local/qwen":        {},
		},
	}
}

// decision builds a completed decision served by `chosen`, with `ladder`
// as the rungs that were configured.
func decision(chosen string, ladder []string, usage domains.TokenUsage) domains.RoutingDecision {
	refs := make([]domains.TargetRef, 0, len(ladder))
	for _, n := range ladder {
		refs = append(refs, domains.TargetRef{Name: n})
	}

	attempts := make([]domains.Attempt, 0, len(ladder))
	for _, n := range ladder {
		a := domains.Attempt{Target: n}
		if n != chosen {
			a.Failure = &domains.AttemptFailure{Kind: "connect"}
		}
		attempts = append(attempts, a)
		if n == chosen {
			break // the ladder stops at the target that served
		}
	}

	return domains.RoutingDecision{
		ID:         domains.NewDecisionID(),
		OccurredAt: augustFifteenth,
		Ladder:     domains.Ladder{Targets: refs},
		Outcome: domains.Outcome{
			Status:   domains.StatusOK,
			Attempts: attempts,
			Usage:    usage,
		},
	}
}

func TestPrice_ActualCost(t *testing.T) {
	p := New(table(), nil)
	d := decision("openai/gpt-5-mini", []string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	if cb == nil {
		t.Fatal("nil breakdown for a priceable decision")
	}
	if want := domains.USD(188_250); cb.Actual != want {
		t.Errorf("actual = %d nanos (%v), want %d (%v)", cb.Actual, cb.Actual, want, want)
	}
	if cb.PriceTableVersion == "" {
		t.Error("no price table version recorded; reports could not be reproduced")
	}
}

// Pricing must be a pure function of the record. If it reached for
// time.Now(), a replay months later would price at whatever rates were
// current then.
func TestPrice_UsesTheRecordsTimeNotNow(t *testing.T) {
	p := New(table(), nil)

	d := decision("openai/gpt-5-mini", []string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 100, Output: 100})
	d.OccurredAt = beforeAnyPrices

	if cb := p.Price(d); cb != nil {
		t.Errorf("priced a decision from before the earliest table: %+v", cb)
	}
}

func TestPrice_IsDeterministic(t *testing.T) {
	p := New(table(), []string{"openai/gpt-5"})
	d := decision("openai/gpt-5-mini",
		[]string{"local/qwen", "openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	first := p.Price(d)
	for i := 0; i < 50; i++ {
		if got := p.Price(d); !reflect.DeepEqual(got, first) {
			t.Fatalf("iteration %d differs:\n got: %+v\nwant: %+v", i, got, first)
		}
	}
}

// These three are the pair from the table tests, one layer up. Nil means
// unpriced; a zero Actual means genuinely free. A report that conflates
// them is wrong in a way nobody would notice.

func TestPrice_ExhaustedLadderHasNoCost(t *testing.T) {
	p := New(table(), nil)

	d := decision("", []string{"openai/gpt-5-mini"}, domains.TokenUsage{})
	d.Outcome.Status = domains.StatusExhausted
	d.Outcome.Attempts = []domains.Attempt{
		{Target: "openai/gpt-5-mini", Failure: &domains.AttemptFailure{Kind: "connect"}},
	}

	if cb := p.Price(d); cb != nil {
		t.Errorf("cost attributed to a request that was never served: %+v", cb)
	}
}

func TestPrice_UnpricedChosenTargetYieldsNil(t *testing.T) {
	p := New(table(), nil)
	d := decision("openai/never-in-the-table", []string{"openai/never-in-the-table"},
		domains.TokenUsage{Input: 100, Output: 100})

	if cb := p.Price(d); cb != nil {
		t.Errorf("cost = %+v, want nil — an unpriced target must not read as free", cb)
	}
}

func TestPrice_FreeTargetPricesAtZero(t *testing.T) {
	p := New(table(), nil)
	d := decision("local/qwen", []string{"local/qwen"},
		domains.TokenUsage{Input: 1_000_000, Output: 1_000_000})

	cb := p.Price(d)

	if cb == nil {
		t.Fatal("nil breakdown; a free target is a priced answer, not an unpriced one")
	}
	if cb.Actual != 0 {
		t.Errorf("actual = %v, want 0", cb.Actual)
	}
}

// A client disconnect still consumed tokens up to the point it happened.
func TestPrice_PartialUsageStillPrices(t *testing.T) {
	p := New(table(), nil)

	d := decision("openai/gpt-5-mini", []string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 5})
	d.Outcome.Status = domains.StatusClientDisconnect

	cb := p.Price(d)

	if cb == nil {
		t.Fatal("a disconnect that consumed tokens must still be priced")
	}
	if cb.Actual == 0 {
		t.Error("actual = 0 despite recorded usage")
	}
}

// --- counterfactuals -------------------------------------------------

func TestPrice_ExcludesTheChosenTargetFromItsOwnComparison(t *testing.T) {
	p := New(table(), []string{"openai/gpt-5-mini", "openai/gpt-5"})
	d := decision("openai/gpt-5-mini",
		[]string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	for _, cf := range cb.Counterfactuals {
		if cf.Target == "openai/gpt-5-mini" {
			t.Fatal("the chosen target appears as its own counterfactual; " +
				"reports would show a delta of zero and read as a bug")
		}
	}
}

func TestPrice_MergesLadderAndConfiguredComparisons(t *testing.T) {
	p := New(table(), []string{"openai/gpt-5"})
	d := decision("openai/gpt-5-mini",
		[]string{"local/qwen", "openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	got := make([]string, 0, len(cb.Counterfactuals))
	for _, cf := range cb.Counterfactuals {
		got = append(got, cf.Target)
	}

	want := []string{"local/qwen", "openai/gpt-5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("counterfactuals = %v, want %v", got, want)
	}
}

func TestPrice_DeduplicatesComparisons(t *testing.T) {
	// The same target appears in both the ladder and the configured set.
	p := New(table(), []string{"openai/gpt-5"})
	d := decision("openai/gpt-5-mini",
		[]string{"openai/gpt-5", "openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	if len(cb.Counterfactuals) != 1 {
		t.Errorf("got %d counterfactuals, want 1: %+v", len(cb.Counterfactuals), cb.Counterfactuals)
	}
}

// One missing comparison must not cost us the actual cost, but the
// report has to know its set is incomplete.
func TestPrice_UnpricedComparisonIsSkippedAndDisclosed(t *testing.T) {
	p := New(table(), []string{"openai/gpt-5", "openai/not-in-the-table"})
	d := decision("openai/gpt-5-mini",
		[]string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	if cb == nil {
		t.Fatal("a missing comparison should not lose the actual cost")
	}
	if cb.Actual == 0 {
		t.Error("actual cost lost")
	}
	if len(cb.Counterfactuals) != 1 {
		t.Errorf("got %d counterfactuals, want 1", len(cb.Counterfactuals))
	}
	if want := []string{"openai/not-in-the-table"}; !reflect.DeepEqual(cb.UnpricedComparisons, want) {
		t.Errorf("unpriced = %v, want %v", cb.UnpricedComparisons, want)
	}
}

// The number the whole product exists to produce.
func TestPrice_FlagshipCounterfactualIsHigher(t *testing.T) {
	p := New(table(), []string{"openai/gpt-5"})
	d := decision("openai/gpt-5-mini",
		[]string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	delta, ok := cb.Delta("openai/gpt-5")
	if !ok {
		t.Fatal("no flagship counterfactual")
	}
	if delta >= 0 {
		t.Errorf("delta = %v, want negative — the cheaper model should cost less", delta)
	}

	t.Logf("actual %v, flagship would have been %v, saved %v",
		cb.Actual, cb.Actual-delta, -delta)
}

func TestPrice_NoComparisonsConfigured(t *testing.T) {
	p := New(table(), nil)
	d := decision("openai/gpt-5-mini", []string{"openai/gpt-5-mini"},
		domains.TokenUsage{Input: 17, Output: 92})

	cb := p.Price(d)

	if len(cb.Counterfactuals) != 0 {
		t.Errorf("counterfactuals = %+v, want none", cb.Counterfactuals)
	}
	if cb.Actual == 0 {
		t.Error("actual cost missing")
	}
}

var _ ports.PricingTable = stubTable{}
var _ = errors.Is
