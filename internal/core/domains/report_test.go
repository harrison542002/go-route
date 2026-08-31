package domains

import "testing"

func TestReportRow_Coverage(t *testing.T) {
	tests := []struct {
		name string
		row  ReportRow
		want float64
	}{
		{"fully priced", ReportRow{Requests: 100}, 1.0},
		{"none priced", ReportRow{Requests: 100, Unpriced: 100}, 0.0},
		{"partly priced", ReportRow{Requests: 100, Unpriced: 25}, 0.75},
		{"no requests", ReportRow{}, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.Coverage(); got != tt.want {
				t.Errorf("Coverage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReportRow_Savings(t *testing.T) {
	row := ReportRow{
		Cost: FromDollars(298.10),
		Comparisons: map[string]Comparison{
			"openai/gpt-5": {Cost: FromDollars(3584.54), Requests: 100},
		},
	}

	delta, ok := row.Savings("openai/gpt-5")
	if !ok {
		t.Fatal("comparison not found")
	}
	if delta >= 0 {
		t.Errorf("delta = %v, want negative — the chosen routing was cheaper", delta)
	}

	if _, ok := row.Savings("never-compared"); ok {
		t.Error("reported savings against a target that was never compared")
	}
}

// A comparison covering fewer requests than the group understates what
// the alternative cost, and must be marked.
func TestComparison_IsPartial(t *testing.T) {
	full := Comparison{Requests: 100}
	partial := Comparison{Requests: 40}

	if full.IsPartial(100) {
		t.Error("full coverage reported as partial")
	}
	if !partial.IsPartial(100) {
		t.Error("partial coverage not detected")
	}
}

func TestUSD_Auto(t *testing.T) {
	tests := []struct {
		nanos USD
		want  string
	}{
		{0, "$0.00"},
		{77_750, "$0.000078"},
		{1_038_250, "$0.001038"},
		{52_400_000, "$0.0524"},
		{1_204_550_000_000, "$1204.55"},
	}

	for _, tt := range tests {
		if got := tt.nanos.Auto(); got != tt.want {
			t.Errorf("USD(%d).Auto() = %q, want %q", tt.nanos, got, tt.want)
		}
	}
}
