package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

func date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func rate(in, out float64) config.RateConfig {
	return config.RateConfig{InputPerMillion: in, OutputPerMillion: out}
}

func perMillion(d float64) domains.PerMillionTokens {
	return domains.PerMillionTokens(domains.FromDollars(d))
}

func twoBlocks() []config.PriceBlock {
	return []config.PriceBlock{
		{
			EffectiveFrom: date("2026-08-01"),
			Rates: map[string]config.RateConfig{
				"openai/gpt-5-mini": rate(0.25, 2.00),
				"local/qwen":        {Free: true},
			},
		},
		{
			EffectiveFrom: date("2026-09-01"),
			Rates: map[string]config.RateConfig{
				"openai/gpt-5-mini": rate(0.20, 1.60),
				"local/qwen":        {Free: true},
			},
		},
	}
}

func newTable(t *testing.T, blocks []config.PriceBlock) *Table {
	t.Helper()
	tbl, err := NewTable(blocks)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tbl
}

func TestTable_RatesAt(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	tests := []struct {
		name        string
		at          string
		wantInput   float64
		wantVersion string
	}{
		{"on the first block's date", "2026-08-01", 0.25, "2026-08-01"},
		{"between blocks", "2026-08-25", 0.25, "2026-08-01"},
		{"the day before the cut", "2026-08-31", 0.25, "2026-08-01"},
		{"on the cut date", "2026-09-01", 0.20, "2026-09-01"},
		{"after the cut", "2026-12-25", 0.20, "2026-09-01"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rates, version, err := tbl.RatesAt(date(tt.at), "openai/gpt-5-mini")
			if err != nil {
				t.Fatalf("RatesAt: %v", err)
			}

			if want := perMillion(tt.wantInput); rates.Input != want {
				t.Errorf("input = %v, want %v", domains.USD(rates.Input), domains.USD(want))
			}
			if version != tt.wantVersion {
				t.Errorf("version = %q, want %q", version, tt.wantVersion)
			}
		})
	}
}

func TestTable_VersionIdentifiesTheBlockUsed(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	_, before, err := tbl.RatesAt(date("2026-08-15"), "openai/gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}
	_, after, err := tbl.RatesAt(date("2026-09-15"), "openai/gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatalf("both lookups reported version %q; the block boundary is not honoured", before)
	}
}

func TestTable_SortsBlocksRegardlessOfConfigOrder(t *testing.T) {
	reversed := []config.PriceBlock{
		{
			EffectiveFrom: date("2026-09-01"),
			Rates:         map[string]config.RateConfig{"openai/gpt-5-mini": rate(0.20, 1.60)},
		},
		{
			EffectiveFrom: date("2026-08-01"),
			Rates:         map[string]config.RateConfig{"openai/gpt-5-mini": rate(0.25, 2.00)},
		},
	}

	tbl := newTable(t, reversed)

	rates, version, err := tbl.RatesAt(date("2026-08-15"), "openai/gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}
	if version != "2026-08-01" {
		t.Errorf("version = %q, want the August block", version)
	}
	if want := perMillion(0.25); rates.Input != want {
		t.Errorf("input = %v, want the August rate", domains.USD(rates.Input))
	}
}

func TestTable_UnknownTargetErrors(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	rates, _, err := tbl.RatesAt(date("2026-08-15"), "openai/never-configured")

	if !errors.Is(err, ports.ErrNoPricing) {
		t.Fatalf("err = %v, want ErrNoPricing", err)
	}
	if !rates.IsFree() {
		t.Error("returned non-zero rates alongside an error")
	}
}

func TestTable_FreeTargetPricesAtZero(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	rates, version, err := tbl.RatesAt(date("2026-08-15"), "local/qwen")
	if err != nil {
		t.Fatalf("a free target must price cleanly, got %v", err)
	}
	if !rates.IsFree() {
		t.Errorf("rates = %+v, want all zero", rates)
	}
	if version == "" {
		t.Error("no version reported for a free target")
	}

	if cost := rates.Cost(domains.TokenUsage{Input: 1_000_000, Output: 1_000_000}); cost != 0 {
		t.Errorf("cost = %v, want 0", cost)
	}
}

func TestTable_BeforeEarliestBlockErrors(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	_, _, err := tbl.RatesAt(date("2026-07-01"), "openai/gpt-5-mini")

	if !errors.Is(err, ports.ErrNoPricing) {
		t.Fatalf("err = %v, want ErrNoPricing", err)
	}
}

func TestTable_TargetAddedInALaterBlock(t *testing.T) {
	blocks := []config.PriceBlock{
		{
			EffectiveFrom: date("2026-08-01"),
			Rates:         map[string]config.RateConfig{"openai/gpt-5-mini": rate(0.25, 2.00)},
		},
		{
			EffectiveFrom: date("2026-09-01"),
			Rates: map[string]config.RateConfig{
				"openai/gpt-5-mini": rate(0.25, 2.00),
				"anthropic/haiku":   rate(0.80, 4.00),
			},
		},
	}
	tbl := newTable(t, blocks)

	if _, _, err := tbl.RatesAt(date("2026-08-15"), "anthropic/haiku"); !errors.Is(err, ports.ErrNoPricing) {
		t.Errorf("err = %v, want ErrNoPricing before the target was priced", err)
	}
	if _, _, err := tbl.RatesAt(date("2026-09-15"), "anthropic/haiku"); err != nil {
		t.Errorf("err = %v, want a price after the target was added", err)
	}
}

func TestTable_Empty(t *testing.T) {
	tbl := newTable(t, nil)

	_, _, err := tbl.RatesAt(time.Now(), "anything")
	if !errors.Is(err, ports.ErrNoPricing) {
		t.Fatalf("err = %v, want ErrNoPricing", err)
	}
}

func TestTable_SingleBlock(t *testing.T) {
	tbl := newTable(t, []config.PriceBlock{{
		EffectiveFrom: date("2026-08-01"),
		Rates:         map[string]config.RateConfig{"openai/gpt-5-mini": rate(0.25, 2.00)},
	}})

	if _, _, err := tbl.RatesAt(date("2026-07-31"), "openai/gpt-5-mini"); !errors.Is(err, ports.ErrNoPricing) {
		t.Errorf("err = %v, want ErrNoPricing the day before the block", err)
	}
	if _, _, err := tbl.RatesAt(date("2030-01-01"), "openai/gpt-5-mini"); err != nil {
		t.Errorf("err = %v; the newest block applies indefinitely forward", err)
	}
}

func TestTable_PricesARealRequest(t *testing.T) {
	tbl := newTable(t, twoBlocks())

	rates, _, err := tbl.RatesAt(date("2026-08-25"), "openai/gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}

	cost := rates.Cost(domains.TokenUsage{Input: 17, Output: 92})

	if want := domains.USD(188_250); cost != want {
		t.Errorf("cost = %d nanos (%v), want %d (%v)", cost, cost, want, want)
	}
}

func TestTable_PriceCutReducesCost(t *testing.T) {
	tbl := newTable(t, twoBlocks())
	usage := domains.TokenUsage{Input: 17, Output: 92}

	before, _, _ := tbl.RatesAt(date("2026-08-25"), "openai/gpt-5-mini")
	after, _, _ := tbl.RatesAt(date("2026-09-25"), "openai/gpt-5-mini")

	if after.Cost(usage) >= before.Cost(usage) {
		t.Errorf("post-cut cost %v is not below pre-cut %v",
			after.Cost(usage), before.Cost(usage))
	}
}

func BenchmarkTable_RatesAt(b *testing.B) {
	tbl, err := NewTable(twoBlocks())
	if err != nil {
		b.Fatal(err)
	}
	at := date("2026-08-15")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = tbl.RatesAt(at, "openai/gpt-5-mini")
	}
}
