package domains

import "fmt"

type Rates struct {
	Input      PerMillionTokens
	Output     PerMillionTokens
	CacheRead  PerMillionTokens
	CacheWrite PerMillionTokens
}

func (r Rates) Cost(u TokenUsage) USD {
	return r.Input.Cost(u.Input) +
		r.Output.Cost(u.Output) +
		r.CacheRead.Cost(u.CacheRead) +
		r.CacheWrite.Cost(u.CacheWrite)
}

func (r Rates) IsFree() bool {
	return r == Rates{}
}

func (r Rates) Validate() error {
	for name, v := range map[string]PerMillionTokens{
		"input":       r.Input,
		"output":      r.Output,
		"cache_read":  r.CacheRead,
		"cache_write": r.CacheWrite,
	} {
		if v < 0 {
			return fmt.Errorf("%s rate is negative: %d", name, v)
		}
	}
	if r.CacheRead > r.Input && r.Input > 0 {
		return fmt.Errorf("cache_read rate (%v) exceeds input rate (%v); likely transposed",
			USD(r.CacheRead), USD(r.Input))
	}
	return nil
}

type CostBreakdown struct {
	Actual              USD
	PriceTableVersion   string
	Counterfactuals     []Counterfactual
	UnpricedComparisons []string
}

type Counterfactual struct {
	Target string
	Cost   USD
}

func (c CostBreakdown) Delta(target string) (USD, bool) {
	for _, cf := range c.Counterfactuals {
		if cf.Target == target {
			return c.Actual - cf.Cost, true
		}
	}
	return 0, false
}
