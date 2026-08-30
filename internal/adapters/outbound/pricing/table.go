package pricing

import (
	"fmt"
	"sort"
	"time"

	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type block struct {
	from    time.Time
	version string
	rates   map[string]domains.Rates
}

type Table struct {
	blocks []block
}

var _ ports.PricingTable = (*Table)(nil)

func NewTable(cfg []config.PriceBlock) (*Table, error) {
	blocks := make([]block, 0, len(cfg))
	for _, b := range cfg {
		rates := make(map[string]domains.Rates, len(b.Rates))
		for name, rc := range b.Rates {
			rates[name] = rc.ToRates()
		}
		blocks = append(blocks, block{
			from:    b.EffectiveFrom,
			version: b.EffectiveFrom.Format("2006-01-02"),
			rates:   rates,
		})
	}

	sort.Slice(blocks, func(i, j int) bool { return blocks[i].from.Before(blocks[j].from) })
	return &Table{blocks: blocks}, nil
}

func (t *Table) RatesAt(at time.Time, target string) (domains.Rates, string, error) {
	if len(t.blocks) == 0 {
		return domains.Rates{}, "", ports.ErrNoPricing
	}

	for i := len(t.blocks) - 1; i >= 0; i-- {
		b := t.blocks[i]
		if b.from.After(at) {
			continue
		}
		r, ok := b.rates[target]
		if !ok {
			return domains.Rates{}, "", fmt.Errorf("%w: target %q not in price table %s",
				ports.ErrNoPricing, target, b.version)
		}
		return r, b.version, nil
	}

	return domains.Rates{}, "", fmt.Errorf("%w: %s predates the earliest price table (%s)",
		ports.ErrNoPricing, at.Format(time.RFC3339), t.blocks[0].version)
}
