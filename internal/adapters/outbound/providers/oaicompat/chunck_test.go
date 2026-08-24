package oaicompat

import (
	"testing"

	"github.com/harrison542002/go-route/internal/core/domains"
)

func TestChunk_Usage(t *testing.T) {
	tests := []struct {
		name string
		data string
		want *domains.TokenUsage
	}{
		{
			name: "no usage field",
			data: `{"choices":[{"delta":{"content":"hi"}}]}`,
			want: nil,
		},
		{
			name: "basic counts",
			data: `{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45}}`,
			want: &domains.TokenUsage{Input: 120, Output: 45},
		},
		{
			// prompt_tokens is the TOTAL including cached. Counting it
			// wholesale overstates cost on any workload with a stable
			// system prefix, which is most of them.
			name: "cached tokens split out of input",
			data: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,
			        "prompt_tokens_details":{"cached_tokens":800}}}`,
			want: &domains.TokenUsage{Input: 200, Output: 50, CacheRead: 800},
		},
		{
			// reasoning_tokens are already inside completion_tokens.
			// Adding them would double count every reasoning request.
			name: "reasoning recorded but not added to output",
			data: `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":500,
			        "completion_tokens_details":{"reasoning_tokens":300}}}`,
			want: &domains.TokenUsage{Input: 10, Output: 500, Reasoning: 300},
		},
		{
			name: "both details blocks",
			data: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,
			        "prompt_tokens_details":{"cached_tokens":900},
			        "completion_tokens_details":{"reasoning_tokens":200}}}`,
			want: &domains.TokenUsage{Input: 100, Output: 500, CacheRead: 900, Reasoning: 200},
		},
		{
			// A provider reporting cached > prompt is broken, but a
			// negative count would corrupt every downstream report.
			name: "cached exceeding prompt clamps to zero",
			data: `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,
			        "prompt_tokens_details":{"cached_tokens":500}}}`,
			want: &domains.TokenUsage{Input: 0, Output: 10, CacheRead: 500},
		},
		{
			name: "details present but empty",
			data: `{"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":20,
			        "prompt_tokens_details":{},"completion_tokens_details":{}}}`,
			want: &domains.TokenUsage{Input: 50, Output: 20},
		},
		{
			name: "zero usage is still reported",
			data: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			want: &domains.TokenUsage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseChunk([]byte(tt.data))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			got := c.usage()

			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %+v", tt.want)
			}
			if *got != *tt.want {
				t.Errorf("\ngot:  %+v\nwant: %+v", *got, *tt.want)
			}
		})
	}
}

func TestChunk_UsageOnly(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			// The chunk our stream_options injection caused to exist.
			name: "usage with empty choices",
			data: `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			want: true,
		},
		{
			// Some providers report usage alongside content. Dropping
			// these would truncate the response.
			name: "usage alongside choices",
			data: `{"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":10}}`,
			want: false,
		},
		{
			name: "content only",
			data: `{"choices":[{"delta":{"content":"x"}}]}`,
			want: false,
		},
		{
			name: "neither usage nor choices",
			data: `{"id":"chatcmpl-9x","object":"chat.completion.chunk"}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseChunk([]byte(tt.data))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := c.usageOnly(); got != tt.want {
				t.Errorf("usageOnly = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseChunk_Malformed(t *testing.T) {
	for _, data := range []string{`{"choices":`, `not json`, ``, `[]`} {
		t.Run(data, func(t *testing.T) {
			if _, err := parseChunk([]byte(data)); err == nil {
				t.Error("want error")
			}
		})
	}
}
