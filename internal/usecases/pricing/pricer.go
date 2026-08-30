package pricing

import (
	"log/slog"
	"sort"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type Pricer struct {
	table   ports.PricingTable
	compare []string
}

func New(table ports.PricingTable, compare []string) *Pricer {
	return &Pricer{table: table, compare: compare}
}

func (p *Pricer) Price(d domains.RoutingDecision) *domains.CostBreakdown {
	chosen := d.Outcome.ChosenTarget()
	if chosen == "" {
		return nil
	}

	rates, version, err := p.table.RatesAt(d.OccurredAt, chosen)
	if err != nil {
		slog.Warn("decision could not be priced",
			"decision_id", d.ID.String(), "target", chosen, "err", err)
		return nil
	}

	cb := &domains.CostBreakdown{
		Actual:            rates.Cost(d.Outcome.Usage),
		PriceTableVersion: version,
	}

	p.addCounterfactuals(d, chosen, cb)
	return cb
}

func (p *Pricer) addCounterfactuals(d domains.RoutingDecision, chosen string, cb *domains.CostBreakdown) {
	for _, target := range p.comparisonTargets(d, chosen) {
		rates, _, err := p.table.RatesAt(d.OccurredAt, target)
		if err != nil {
			cb.UnpricedComparisons = append(cb.UnpricedComparisons, target)
			continue
		}
		cb.Counterfactuals = append(cb.Counterfactuals, domains.Counterfactual{
			Target: target,
			Cost:   rates.Cost(d.Outcome.Usage),
		})
	}
}

func (p *Pricer) comparisonTargets(d domains.RoutingDecision, chosen string) []string {
	seen := make(map[string]struct{}, len(d.Ladder.Targets)+len(p.compare))

	add := func(name string) {
		if name == "" || name == chosen {
			return
		}
		seen[name] = struct{}{}
	}

	for _, ref := range d.Ladder.Targets {
		add(ref.Name)
	}

	for _, name := range p.compare {
		add(name)
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
