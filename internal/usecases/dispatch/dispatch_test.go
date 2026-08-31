package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
	"go.uber.org/mock/gomock"
)

// recorder captures the downstream call sequence so tests can assert
// both what was sent and in what order.
type recorder struct {
	ops       []string // "commit", "send", "error"
	sent      []ports.StreamEvent
	commitErr error
	sendErrAt int // return an error from the Nth Send; -1 to never
}

func newRecorder() *recorder { return &recorder{sendErrAt: -1} }

func (r *recorder) Commit() error {
	r.ops = append(r.ops, "commit")
	return r.commitErr
}

func (r *recorder) Send(ev ports.StreamEvent) error {
	if r.sendErrAt >= 0 && len(r.sent) == r.sendErrAt {
		return errors.New("client gone")
	}
	r.ops = append(r.ops, "send")
	r.sent = append(r.sent, ev)
	return nil
}

func (r *recorder) SendError(error) error {
	r.ops = append(r.ops, "error")
	return nil
}

func (r *recorder) count(op string) int {
	n := 0
	for _, o := range r.ops {
		if o == op {
			n++
		}
	}
	return n
}

// --- builders --------------------------------------------------------

// scriptedReader returns a MockStreamReader that yields evs in order.
// After the script is exhausted it returns endErr; pass io.EOF for a
// clean end or any other error to model a mid-stream failure.
func scriptedReader(ctrl *gomock.Controller, evs []ports.StreamEvent, endErr error) *mocks.MockStreamReader {
	r := mocks.NewMockStreamReader(ctrl)

	i := 0
	r.EXPECT().Next().DoAndReturn(func() (ports.StreamEvent, error) {
		if i >= len(evs) {
			return ports.StreamEvent{}, endErr
		}
		ev := evs[i]
		i++
		return ev, nil
	}).AnyTimes()

	r.EXPECT().Close().Return(nil).AnyTimes()
	return r
}

// streamingProvider yields evs and then endErr.
func streamingProvider(ctrl *gomock.Controller, name string, evs []ports.StreamEvent, endErr error) *mocks.MockProvider {
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return(name).AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).
		Return(scriptedReader(ctrl, evs, endErr), nil).AnyTimes()
	return p
}

// failingProvider never connects.
func failingProvider(ctrl *gomock.Controller, name string, err error) *mocks.MockProvider {
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return(name).AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).Return(nil, err).AnyTimes()
	return p
}

// unusedProvider asserts it is never dispatched to.
func unusedProvider(ctrl *gomock.Controller, name string) *mocks.MockProvider {
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return(name).AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).Times(0)
	return p
}

// --- helpers ---------------------------------------------------------
var fixedNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// clock advances 10ms per call so durations are deterministic but non-zero.
func clock() func() time.Time {
	n := 0
	return func() time.Time {
		t := fixedNow.Add(time.Duration(n) * 10 * time.Millisecond)
		n++
		return t
	}
}

func events(n int) []ports.StreamEvent {
	out := make([]ports.StreamEvent, n)
	for i := range out {
		out[i] = ports.StreamEvent{Raw: []byte(`{"choices":[{"delta":{"content":"x"}}]}`)}
	}
	out[n-1].Terminal = true
	return out
}

func run(t *testing.T, ladder []ports.Target, req *ports.ProviderRequest, rec *recorder) domains.Outcome {
	t.Helper()
	if req == nil {
		req = &ports.ProviderRequest{Model: "m", Stream: true}
	}
	out := New(clock()).Run(context.Background(), ladder, req, rec)
	if err := Validate(out); err != nil {
		t.Fatalf("incoherent outcome: %v\n%+v", err, out)
	}
	return out
}

func TestRun_FirstTargetSucceeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := streamingProvider(ctrl, "openai", events(3), io.EOF)
	rec := newRecorder()

	out := run(t, []ports.Target{{Provider: p, Model: "gpt-5-mini"}}, nil, rec)

	if out.Status != domains.StatusOK {
		t.Errorf("status = %q, want ok", out.Status)
	}
	if len(out.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(out.Attempts))
	}
	if out.Attempts[0].Failure != nil {
		t.Errorf("successful attempt recorded a failure: %+v", out.Attempts[0].Failure)
	}
	if rec.count("commit") != 1 {
		t.Errorf("commit called %d times, want exactly 1", rec.count("commit"))
	}
	if len(rec.sent) != 3 {
		t.Errorf("sent %d events, want 3", len(rec.sent))
	}
	if rec.ops[0] != "commit" {
		t.Errorf("first op = %q, want commit before any send", rec.ops[0])
	}
}

func TestRun_FailoverBeforeCommit(t *testing.T) {
	ctrl := gomock.NewController(t)
	dead := failingProvider(ctrl, "dead", errors.New("connection refused"))
	alive := streamingProvider(ctrl, "alive", events(2), io.EOF)
	rec := newRecorder()

	out := run(t, []ports.Target{{Provider: dead, Model: "a"}, {Provider: alive, Model: "b"}}, nil, rec)

	if out.Status != domains.StatusOK {
		t.Errorf("status = %q, want ok", out.Status)
	}
	if len(out.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 — the failed ports.Target must be recorded", len(out.Attempts))
	}
	if out.Attempts[0].Failure == nil {
		t.Error("first attempt should record a failure")
	}
	if out.Attempts[1].Failure != nil {
		t.Error("second attempt succeeded and should record no failure")
	}
	if rec.count("commit") != 1 {
		t.Errorf("commit called %d times, want 1", rec.count("commit"))
	}
}

// The core guarantee: if nothing yields a first event, the status line is
// never written, so the caller can still return a real HTTP error.
func TestRun_AllTargetsFail_NeverCommits(t *testing.T) {
	ctrl := gomock.NewController(t)

	var ladder []ports.Target
	for _, name := range []string{"a", "b", "c"} {
		ladder = append(ladder, ports.Target{Provider: failingProvider(ctrl, name, errors.New("unreachable")), Model: name})
	}
	rec := newRecorder()

	out := run(t, ladder, nil, rec)

	if out.Status != domains.StatusExhausted {
		t.Errorf("status = %q, want exhausted", out.Status)
	}
	if len(out.Attempts) != 3 {
		t.Errorf("attempts = %d, want 3", len(out.Attempts))
	}
	if len(rec.ops) != 0 {
		t.Fatalf("nothing may be written downstream when no ports.Target commits, got %v", rec.ops)
	}
	if out.TTFTMs != 0 {
		t.Errorf("TTFT = %d, want 0 — no first token was ever received", out.TTFTMs)
	}
}

// Post-commit, the ladder is over.
func TestRun_FailureAfterCommit_DoesNotFailOver(t *testing.T) {
	ctrl := gomock.NewController(t)
	flaky := streamingProvider(ctrl, "flaky", events(3)[:2], errors.New("upstream died"))
	backup := unusedProvider(ctrl, "backup") // Times(0) enforces this
	rec := newRecorder()

	out := run(t, []ports.Target{{Provider: flaky, Model: "a"}, {Provider: backup, Model: "b"}}, nil, rec)

	if out.Status != domains.StatusTruncated {
		t.Errorf("status = %q, want truncated", out.Status)
	}
	if len(out.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1 — the backup must never be tried", len(out.Attempts))
	}
	if rec.count("error") != 1 {
		t.Errorf("SendError called %d times, want 1", rec.count("error"))
	}
}

func TestRun_NonRetryableStopsLadder(t *testing.T) {
	ctrl := gomock.NewController(t)
	bad := failingProvider(ctrl, "bad", &ports.ProviderError{
		Kind: ports.FailureAuth, Message: "invalid api key", Retryable: false,
	})
	other := unusedProvider(ctrl, "other")

	out := run(t, []ports.Target{{Provider: bad, Model: "a"}, {Provider: other, Model: "b"}}, nil, newRecorder())

	if len(out.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1 — an auth failure must not walk the ladder", len(out.Attempts))
	}
	if out.Status != domains.StatusExhausted {
		t.Errorf("status = %q, want exhausted", out.Status)
	}
}

// Anthropic reports cumulative output tokens across two events; summing
// them would inflate every streamed request's cost.
func TestRun_UsageOverwritesNotAccumulates(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := streamingProvider(ctrl, "anthropic", []ports.StreamEvent{
		{Raw: []byte("a"), Usage: &domains.TokenUsage{Input: 100}},
		{Raw: []byte("b"), Usage: &domains.TokenUsage{Input: 100, Output: 50}},
		{Raw: []byte("c"), Usage: &domains.TokenUsage{Input: 100, Output: 120}, Terminal: true},
	}, io.EOF)

	out := run(t, []ports.Target{{Provider: p, Model: "m"}}, nil, newRecorder())

	if out.Usage.Input != 100 {
		t.Errorf("input = %d, want 100 (overwritten, not summed to 300)", out.Usage.Input)
	}
	if out.Usage.Output != 120 {
		t.Errorf("output = %d, want 120 (last value, not 170)", out.Usage.Output)
	}
}

func TestRun_UsageOnlyChunk(t *testing.T) {
	script := []ports.StreamEvent{
		{Raw: []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)},
		{Raw: []byte(`{"choices":[],"usage":{}}`), Usage: &domains.TokenUsage{Input: 12, Output: 8}, UsageOnly: true},
		{Raw: []byte("[DONE]"), Terminal: true},
	}

	t.Run("stripped when the client did not ask", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		p := streamingProvider(ctrl, "openai", script, io.EOF)
		rec := newRecorder()

		out := run(t, []ports.Target{{Provider: p, Model: "m"}},
			&ports.ProviderRequest{Model: "m", Stream: true, WantsUsage: false}, rec)

		if len(rec.sent) != 2 {
			t.Errorf("sent %d events, want 2 — the injected chunk must be hidden", len(rec.sent))
		}
		if out.Usage.Input != 12 {
			t.Errorf("usage was dropped along with the chunk: %+v", out.Usage)
		}
	})

	t.Run("forwarded when the client asked", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		p := streamingProvider(ctrl, "openai", script, io.EOF)
		rec := newRecorder()

		run(t, []ports.Target{{Provider: p, Model: "m"}},
			&ports.ProviderRequest{Model: "m", Stream: true, WantsUsage: true}, rec)

		if len(rec.sent) != 3 {
			t.Errorf("sent %d events, want 3 — the client asked for usage", len(rec.sent))
		}
	})
}

func TestRun_ClientDisconnectDuringRelay(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := streamingProvider(ctrl, "openai", events(5), io.EOF)
	rec := newRecorder()
	rec.sendErrAt = 2

	out := run(t, []ports.Target{{Provider: p, Model: "m"}}, nil, rec)

	if out.Status != domains.StatusClientDisconnect {
		t.Errorf("status = %q, want client_disconnect", out.Status)
	}
	if len(out.Attempts) != 1 {
		t.Error("a disconnect is not a provider failure and must not retry")
	}
}

func TestRun_CommitFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := streamingProvider(ctrl, "openai", events(3), io.EOF)
	rec := newRecorder()
	rec.commitErr = errors.New("broken pipe")

	out := run(t, []ports.Target{{Provider: p, Model: "m"}}, nil, rec)

	if out.Status != domains.StatusClientDisconnect {
		t.Errorf("status = %q, want client_disconnect", out.Status)
	}
	if len(rec.sent) != 0 {
		t.Error("nothing should be sent after a failed commit")
	}
}

func TestRun_EmptyLadder(t *testing.T) {
	out := run(t, nil, nil, newRecorder())

	if out.Status != domains.StatusExhausted {
		t.Errorf("status = %q, want exhausted", out.Status)
	}
	if len(out.Attempts) != 0 {
		t.Errorf("attempts = %d, want 0", len(out.Attempts))
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return("openai").AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).Return(nil, context.Canceled)

	rec := newRecorder()
	out := New(clock()).Run(context.Background(), []ports.Target{{Provider: p, Model: "m"}},
		&ports.ProviderRequest{Model: "m"}, rec)

	if len(rec.ops) != 0 {
		t.Error("a cancelled context must not commit")
	}
	if len(out.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1 — cancellation is not retryable", len(out.Attempts))
	}
}

// The reader must be closed even when the attempt fails before commit,
// or a failing ladder leaks one upstream connection per ports.Target.
func TestRun_ReaderClosedOnPreCommitFailure(t *testing.T) {
	ctrl := gomock.NewController(t)

	rdr := mocks.NewMockStreamReader(ctrl)
	rdr.EXPECT().Next().Return(ports.StreamEvent{}, errors.New("upstream hung up"))
	rdr.EXPECT().Close().Return(nil).Times(1)

	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return("leaky").AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).Return(rdr, nil)

	run(t, []ports.Target{{Provider: p, Model: "m"}}, nil, newRecorder())
}
