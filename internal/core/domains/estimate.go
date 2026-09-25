package domains

// PromptSize is the part of a request body that predicts its prompt token
// count, measured at ingress so nothing downstream has to parse the body again.
type PromptSize struct {
	// TextBytes is the length of every piece of text the model will read:
	// message content, text parts, and tool definitions.
	TextBytes int

	// Messages is the number of chat messages. Each carries framing the
	// provider tokenises on top of its content.
	Messages int

	// MediaParts counts non-text content parts (images, audio, files). Their
	// base64 length says nothing about their token cost, so they are charged a
	// flat amount instead.
	MediaParts int
}

const (
	// bytesPerToken is the usual rule of thumb for English with BPE
	// tokenisers. It overestimates code and underestimates CJK; being a few
	// percent out is fine because reconciliation corrects it.
	bytesPerToken = 4

	// messageOverheadTokens is the role and delimiter framing each chat
	// message costs.
	messageOverheadTokens = 4

	// mediaPartTokens is a flat charge per image or other media part, roughly
	// a high-detail image. Counting the base64 bytes instead would charge a
	// single photo hundreds of thousands of tokens.
	mediaPartTokens = 1000
)

// EstimateTokens sizes a request before dispatch: the worst case it is
// reserved at.
func EstimateTokens(p PromptSize, maxOutput, choices, defaultMaxOutput int) TokenUsage {
	input := (p.TextBytes+bytesPerToken-1)/bytesPerToken +
		p.Messages*messageOverheadTokens +
		p.MediaParts*mediaPartTokens

	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	if choices < 1 {
		choices = 1
	}

	return TokenUsage{Input: input, Output: maxOutput * choices}
}

// Total is every token a request consumed. Reasoning is deliberately left out:
// providers report it as a subset of Output, so adding it would double count.
func (u TokenUsage) Total() int {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}
