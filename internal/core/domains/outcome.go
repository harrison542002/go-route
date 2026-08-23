package domains

import "time"

type Status string

const (
	StatusOK               Status = "ok"
	StatusExhausted        Status = "exhausted" // every target failed pre-commit
	StatusTruncated        Status = "truncated" // died post-commit
	StatusClientDisconnect Status = "client_disconnect"
	StatusPolicyBlocked    Status = "policy_blocked"
)

// Attempt records one target in the failover ladder, successful or not.
type Attempt struct {
	Target     string
	StartedAt  time.Time
	DurationMs int
	Failure    *AttemptFailure
}

type AttemptFailure struct {
	Kind       string
	StatusCode int
	Message    string
	Retryable  bool
}

type Outcome struct {
	Status   Status
	Attempts []Attempt
	Usage    TokenUsage
	TTFTMs   int
	TotalMs  int
}
