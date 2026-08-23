package ports

type FailureKind int

const (
	FailureUnknown    FailureKind = iota
	FailureConnect                // never reached the provider — always retryable
	FailureAuth                   // bad key — retrying the same target is pointless
	FailureRateLimit              // 429 — try elsewhere
	FailureUpstream               // 5xx
	FailureBadRequest             // 4xx from a malformed request — never retry
	FailureTimeout
	FailureTruncated // died mid-stream — post-commit, never retryable
)

func (k FailureKind) String() string {
	switch k {
	case FailureConnect:
		return "connect"
	case FailureAuth:
		return "auth"
	case FailureRateLimit:
		return "rate limit"
	case FailureUpstream:
		return "upstream"
	case FailureBadRequest:
		return "bad request"
	case FailureTimeout:
		return "timeout"
	case FailureTruncated:
		return "died mid-stream"
	default:
		return "unknown"
	}
}

type ProviderError struct {
	Kind       FailureKind
	Provider   string
	StatusCode int
	Message    string
	Retryable  bool
	Err        error
}

func (e *ProviderError) Error() string { return e.Message }
func (e *ProviderError) Unwrap() error { return e.Err }
