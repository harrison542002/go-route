package oaicompat

import (
	"encoding/json"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// chunk is the subset of an OpenAI streaming chunk that matters for
// metering.
type chunk struct {
	Choices []json.RawMessage `json:"choices"`
	Usage   *usageJSON        `json:"usage"`
}

type usageJSON struct {
	PromptTokens            int                     `json:"prompt_tokens"`
	CompletionTokens        int                     `json:"completion_tokens"`
	PromptTokensDetails     *promptTokenDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionTokenDetails `json:"completion_tokens_details"`
}

type promptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func parseChunk(data []byte) (chunk, error) {
	var c chunk
	err := json.Unmarshal(data, &c)
	return c, err
}

// usage converts reported counts into domain terms.
//
// Cached input tokens are billed far below fresh ones, so they must be
// separated: counting prompt_tokens wholesale overstates cost on any
// workload with a stable system prefix, which is most of them.
//
// Reasoning tokens are already inside completion_tokens. They are
// recorded separately for reporting and must NOT be added to Output.
func (c chunk) usage() *domains.TokenUsage {
	if c.Usage == nil {
		return nil
	}

	cached := 0
	if c.Usage.PromptTokensDetails != nil {
		cached = c.Usage.PromptTokensDetails.CachedTokens
	}

	reasoning := 0
	if c.Usage.CompletionTokensDetails != nil {
		reasoning = c.Usage.CompletionTokensDetails.ReasoningTokens
	}

	input := c.Usage.PromptTokens - cached
	if input < 0 {
		// Defensive: a provider reporting cached > prompt is broken, but
		// a negative token count would corrupt every downstream report.
		input = 0
	}

	return &domains.TokenUsage{
		Input:     input,
		Output:    c.Usage.CompletionTokens,
		CacheRead: cached,
		Reasoning: reasoning,
	}
}

// usageOnly reports whether this chunk carries usage and nothing else.
// It is the chunk our stream_options injection caused to exist; the
// dispatcher hides it from clients who did not ask for usage.
func (c chunk) usageOnly() bool {
	return c.Usage != nil && len(c.Choices) == 0
}
