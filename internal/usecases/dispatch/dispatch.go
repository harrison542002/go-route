package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type Target struct {
	Provider ports.Provider
	Model    string
	Ref      domains.TargetRef
}

func (t Target) String() string {
	return t.Provider.Name() + "/" + t.Model
}

type Dispatcher struct {
	now func() time.Time
}

func New(now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{now: now}
}

func (d *Dispatcher) Run(
	ctx context.Context,
	ladder []Target,
	req *ports.ProviderRequest,
	out ports.ClientStream,
) domains.Outcome {

	start := d.now()
	var outcome domains.Outcome

	for _, target := range ladder {
		attemptStart := d.now()

		done, err := d.tryTarget(ctx, target, req, out, &outcome, start, attemptStart)
		if done {
			outcome.TotalMs = msBetween(start, d.now())
			return outcome
		}

		outcome.Attempts = append(outcome.Attempts,
			d.failedAttempt(target, attemptStart, err))

		if !retryable(err) {
			break
		}
	}

	outcome.Status = domains.StatusExhausted
	outcome.TotalMs = msBetween(start, d.now())
	return outcome
}

// tryTarget makes one attempt. It returns done=true when the ladder is
// over — either because the stream was relayed, or because the client
// went away — in which case outcome is final.
func (d *Dispatcher) tryTarget(
	ctx context.Context,
	target Target,
	req *ports.ProviderRequest,
	out ports.ClientStream,
	outcome *domains.Outcome,
	start, attemptStart time.Time,
) (done bool, err error) {

	attemptReq := *req
	attemptReq.Model = target.Model
	reader, err := target.Provider.Stream(ctx, &attemptReq)
	if err != nil {
		return false, err
	}
	defer reader.Close()

	// Nothing has been written downstream yet, so a failure reading the
	// first event is still recoverable by trying the next target.
	first, err := reader.Next()
	if err != nil {
		return false, err
	}

	outcome.TTFTMs = msBetween(start, d.now())
	outcome.Attempts = append(outcome.Attempts, domains.Attempt{
		Target:     target.String(),
		StartedAt:  attemptStart,
		DurationMs: msBetween(attemptStart, d.now()),
	})

	if err := out.Commit(); err != nil {
		outcome.Status = domains.StatusClientDisconnect
		return true, nil
	}

	d.relay(reader, first, req, out, outcome)
	return true, nil
}

// relay forwards events until the stream ends. first has already been read
// and is handled before the loop advances.
func (d *Dispatcher) relay(
	reader ports.StreamReader,
	first ports.StreamEvent,
	req *ports.ProviderRequest,
	out ports.ClientStream,
	outcome *domains.Outcome) {
	ev := first

	for {
		if ev.Usage != nil {
			outcome.Usage = *ev.Usage
		}

		skip := ev.UsageOnly && !req.WantsUsage

		if !skip {
			if err := out.Send(ev); err != nil {
				outcome.Status = domains.StatusClientDisconnect
				return
			}
		}

		if ev.Terminal {
			outcome.Status = domains.StatusOK
			return
		}

		var err error
		ev, err = reader.Next()
		if err == nil {
			continue
		}

		switch {
		case errors.Is(err, io.EOF):
			outcome.Status = domains.StatusOK
		case errors.Is(err, context.Canceled),
			errors.Is(err, context.DeadlineExceeded):
			outcome.Status = domains.StatusClientDisconnect
		default:
			outcome.Status = domains.StatusTruncated
			_ = out.SendError(err)
		}
		return
	}
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var pe *ports.ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return true
}

func (d *Dispatcher) failedAttempt(t Target, startedAt time.Time, err error) domains.Attempt {
	failure := domains.AttemptFailure{
		Kind:      ports.FailureUnknown.String(),
		Message:   err.Error(),
		Retryable: true,
	}

	var pe *ports.ProviderError
	if errors.As(err, &pe) {
		failure = domains.AttemptFailure{
			Kind:       pe.Kind.String(),
			StatusCode: pe.StatusCode,
			Message:    pe.Message,
			Retryable:  pe.Retryable,
		}
	}

	switch {
	case errors.Is(err, context.Canceled):
		failure.Kind = "client_disconnect"
		failure.Retryable = false
	case errors.Is(err, context.DeadlineExceeded):
		failure.Kind = ports.FailureTimeout.String()
		failure.Retryable = false
	}

	return domains.Attempt{
		Target:     t.String(),
		StartedAt:  startedAt,
		DurationMs: msBetween(startedAt, d.now()),
		Failure:    &failure,
	}
}

func msBetween(from, to time.Time) int {
	return int(to.Sub(from).Milliseconds())
}

func Validate(o domains.Outcome) error {
	if o.Status == "" {
		return fmt.Errorf("outcome has no status")
	}
	if o.Status == domains.StatusExhausted && o.TTFTMs != 0 {
		return fmt.Errorf("exhausted outcome has a first-token time")
	}
	return nil
}
