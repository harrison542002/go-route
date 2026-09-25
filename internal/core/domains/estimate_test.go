package domains

import "testing"

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name      string
		prompt    PromptSize
		maxOutput int
		choices   int
		want      TokenUsage
	}{
		{
			name:      "text rounds up to whole tokens",
			prompt:    PromptSize{TextBytes: 9, Messages: 1},
			maxOutput: 100,
			want:      TokenUsage{Input: 3 + 4, Output: 100},
		},
		{
			name:   "no ceiling falls back to the default",
			prompt: PromptSize{TextBytes: 400, Messages: 2},
			want:   TokenUsage{Input: 100 + 8, Output: 4096},
		},
		{
			// A photo's base64 is megabytes; charging it by length would
			// lock a tenant out on one image.
			name:      "media is a flat charge, not its byte length",
			prompt:    PromptSize{TextBytes: 0, Messages: 1, MediaParts: 2},
			maxOutput: 10,
			want:      TokenUsage{Input: 4 + 2000, Output: 10},
		},
		{
			name:      "every choice can generate up to the ceiling",
			prompt:    PromptSize{},
			maxOutput: 50,
			choices:   3,
			want:      TokenUsage{Input: 0, Output: 150},
		},
		{
			name:      "a negative ceiling is treated as absent",
			maxOutput: -5,
			want:      TokenUsage{Output: 4096},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.prompt, tt.maxOutput, tt.choices, 4096)
			if got != tt.want {
				t.Errorf("EstimateTokens = %+v, want %+v", got, tt.want)
			}
			if got.Total() != tt.want.Input+tt.want.Output {
				t.Errorf("Total = %d", got.Total())
			}
		})
	}
}

// Cached tokens are split out of Input by the provider adapter, so a
// total that forgot them would under-count every cached prompt.
func TestTokenUsage_TotalCountsEveryBucket(t *testing.T) {
	u := TokenUsage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 5, Reasoning: 15}
	if got := u.Total(); got != 65 {
		t.Errorf("Total = %d, want 65 (reasoning is already inside output)", got)
	}
}
