package domains

import (
	"fmt"
	"time"
)

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
	Target     string          `json:"target"`
	StartedAt  time.Time       `json:"started_at"`
	DurationMs int             `json:"duration_ms"`
	Failure    *AttemptFailure `json:"failure,omitempty"`
}

type AttemptFailure struct {
	Kind       string `json:"kind"`
	StatusCode int    `json:"status_code,omitempty"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
}

type Outcome struct {
	Status   Status
	Attempts []Attempt
	Usage    TokenUsage
	TTFTMs   int
	TotalMs  int
}

func (o Outcome) ChosenTarget() string {
	if len(o.Attempts) == 0 {
		return ""
	}
	last := o.Attempts[len(o.Attempts)-1]
	if last.Failure != nil {
		return ""
	}
	return last.Target
}

func Validate(o Outcome) error {
	if o.Status == "" {
		return fmt.Errorf("outcome has no status")
	}
	if o.Status == StatusExhausted && o.TTFTMs != 0 {
		return fmt.Errorf("exhausted outcome has a first-token time")
	}
	if o.Status == StatusExhausted && o.ChosenTarget() != "" {
		return fmt.Errorf("exhausted outcome names a chosen target %q", o.ChosenTarget())
	}
	if o.Status != StatusExhausted && len(o.Attempts) > 0 && o.ChosenTarget() == "" {
		return fmt.Errorf("status %q but no attempt succeeded", o.Status)
	}
	return nil
}
