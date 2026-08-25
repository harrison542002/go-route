package domains

import "testing"

func TestOutcome_ChosenTarget(t *testing.T) {
	tests := []struct {
		name string
		o    Outcome
		want string
	}{
		{"no attempts", Outcome{}, ""},
		{
			name: "all failed",
			o: Outcome{Attempts: []Attempt{
				{Target: "a", Failure: &AttemptFailure{}},
				{Target: "b", Failure: &AttemptFailure{}},
			}},
			want: "",
		},
		{
			name: "failover then success",
			o: Outcome{Attempts: []Attempt{
				{Target: "a", Failure: &AttemptFailure{}},
				{Target: "b"},
			}},
			want: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.o.ChosenTarget(); got != tt.want {
				t.Errorf("ChosenTarget = %q, want %q", got, tt.want)
			}
		})
	}
}
