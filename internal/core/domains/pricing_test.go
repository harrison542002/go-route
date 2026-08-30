package domains

import "testing"

var gpt5Mini = Rates{
	Input:      PerMillionTokens(FromDollars(0.25)),
	Output:     PerMillionTokens(FromDollars(2.00)),
	CacheRead:  PerMillionTokens(FromDollars(0.025)),
	CacheWrite: PerMillionTokens(FromDollars(0.3125)),
}

func TestRates_Cost(t *testing.T) {
	tests := []struct {
		name  string
		rates Rates
		usage TokenUsage
		want  USD
	}{
		{
			name:  "no usage costs nothing",
			rates: gpt5Mini,
			usage: TokenUsage{},
			want:  0,
		},
		{
			// 1M input at $0.25 + 1M output at $2.00
			name:  "one million of each",
			rates: gpt5Mini,
			usage: TokenUsage{Input: 1_000_000, Output: 1_000_000},
			want:  FromDollars(2.25),
		},
		{
			// 17 × 250 nanos + 92 × 2000 nanos = 4250 + 184000
			name:  "a realistic small request",
			rates: gpt5Mini,
			usage: TokenUsage{Input: 17, Output: 92},
			want:  USD(188_250),
		},
		{
			// Reasoning tokens are already inside completion_tokens.
			// Including them here would double count.
			name:  "reasoning tokens are not billed twice",
			rates: gpt5Mini,
			usage: TokenUsage{Input: 10, Output: 500, Reasoning: 300},
			want:  gpt5Mini.Cost(TokenUsage{Input: 10, Output: 500}),
		},
		{
			name:  "a free target costs nothing however much it is used",
			rates: Rates{},
			usage: TokenUsage{Input: 1_000_000, Output: 1_000_000},
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rates.Cost(tt.usage); got != tt.want {
				t.Errorf("cost = %v (%d nanos), want %v (%d nanos)",
					got, got, tt.want, tt.want)
			}
		})
	}
}
func TestRates_CachedTokensCostLess(t *testing.T) {
	const total = 10_000

	fresh := gpt5Mini.Cost(TokenUsage{Input: total, Output: 100})
	cached := gpt5Mini.Cost(TokenUsage{Input: total / 5, CacheRead: total * 4 / 5, Output: 100})

	if cached >= fresh {
		t.Fatalf("cached (%v) is not cheaper than fresh (%v)", cached, fresh)
	}

	saving := float64(fresh-cached) / float64(fresh)
	if saving < 0.5 {
		t.Errorf("saving is only %.0f%%; the cache rate is not being applied", saving*100)
	}
}

func TestRates_SmallRequestsAreNotFree(t *testing.T) {
	cost := gpt5Mini.Cost(TokenUsage{Input: 1, Output: 1})

	if cost == 0 {
		t.Fatal("a one-token request priced at zero; check for divide-before-multiply")
	}
	if want := USD(2250); cost != want {
		t.Errorf("cost = %d nanos, want %d", cost, want)
	}
}

func TestRates_AccumulatesExactly(t *testing.T) {
	const n = 1_000_000
	per := gpt5Mini.Cost(TokenUsage{Input: 17, Output: 92})

	var total USD
	for i := 0; i < n; i++ {
		total += per
	}

	if want := per * n; total != want {
		t.Errorf("total = %d, want %d — arithmetic is not exact", total, want)
	}
}

func TestRates_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rates   Rates
		wantErr bool
	}{
		{"valid", gpt5Mini, false},
		{"free target", Rates{}, false},
		{
			name:    "negative input",
			rates:   Rates{Input: PerMillionTokens(-1)},
			wantErr: true,
		},
		{
			name: "cache read above input",
			rates: Rates{
				Input:     PerMillionTokens(FromDollars(0.25)),
				CacheRead: PerMillionTokens(FromDollars(2.50)),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rates.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestCostBreakdown_Delta(t *testing.T) {
	cb := CostBreakdown{
		Actual: FromDollars(0.000188),
		Counterfactuals: []Counterfactual{
			{Target: "openai/gpt-5", Cost: FromDollars(0.000941)},
		},
	}

	delta, ok := cb.Delta("openai/gpt-5")
	if !ok {
		t.Fatal("counterfactual not found")
	}
	if delta >= 0 {
		t.Errorf("delta = %v, want negative — the chosen target was cheaper", delta)
	}

	if _, ok := cb.Delta("nonexistent"); ok {
		t.Error("reported a delta for a target with no counterfactual")
	}
}
