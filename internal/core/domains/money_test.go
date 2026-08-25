package domains

import "testing"

func TestUSD_NoFloatDrift(t *testing.T) {
	// Ten million requests at a fraction of a cent each. Summed as
	// float64 dollars this accumulates visible error.
	const per = USD(21_000) // $0.000021
	var total USD
	for i := 0; i < 10_000_000; i++ {
		total += per
	}
	if want := USD(210_000_000_000); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
}

func TestPerMillionTokens_Cost(t *testing.T) {
	rate := PerMillionTokens(FromDollars(0.25)) // $0.25 / 1M tokens

	if got := rate.Cost(1_000_000); got != FromDollars(0.25) {
		t.Errorf("1M tokens = %v, want $0.25", got)
	}
	// The case that breaks a naive divide-first implementation.
	if got := rate.Cost(1); got != USD(250) {
		t.Errorf("1 token = %d nanos, want 250", got)
	}
}
