package proxyapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
)

func quotaHandler(r Router, res Resolver, q ports.QuotaEnforcer) (http.Handler, *recordingSink) {
	s := &recordingSink{}
	return mounted(NewHandler(r, res, dispatch.New(testNowFn), s, q, testNowFn),
		stubAuth{tenant: "acme"}), s
}

func breach(kind domains.WindowKind, used, requested, limit domains.USD) domains.QuotaBreach {
	w, err := domains.ClockWindow(kind, testNowFn())
	if err != nil {
		panic(err)
	}
	return domains.QuotaBreach{Window: w, Used: used, Requested: requested, Limit: limit}
}

// A blocked request must be refused before any upstream is paid for,
// with enough in the response for the caller to act on without asking.
func TestCompletions_QuotaExceeded(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := unusedProvider(ctrl, "openai")
	r, res, _ := routing(ctrl, []ports.Target{target(p, "openai", "gpt-5")}, nil)

	b := breach(domains.WindowMinute, domains.FromDollars(0.99), domains.FromDollars(0.05), domains.FromDollars(1))
	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).
		Return(ports.Reservation{}, fmt.Errorf("wrapped: %w", &ports.QuotaExceededError{Breach: b}))
	q.EXPECT().Reconcile(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	h, sink := quotaHandler(r, res, q)
	resp := post(t, h, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	// testNowFn is exactly on the hour, so the minute resets in 60s.
	headers := map[string]string{
		"Retry-After":    "60",
		"X-Should-Retry": "true",
		"Content-Type":   "application/json",
	}
	for k, want := range headers {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if id := resp.Header.Get("X-Go-Route-Decision-Id"); !strings.HasPrefix(id, "dec_") {
		t.Errorf("decision ID = %q; the refusal is recorded, so explain must be able to find it", id)
	}

	env := decodeError(t, resp)
	if env.Error.Type != "quota_exceeded" || env.Error.Code == nil || *env.Error.Code != "quota_exceeded" {
		t.Errorf("type/code = %q/%v", env.Error.Type, env.Error.Code)
	}
	// The message is the whole answer: no client parses a structured body, so
	// everything a caller needs to act on has to be legible in this sentence.
	for _, want := range []string{"spend", "minute", "$0.9900", "$1.00", "$0.0500", "2026-08-24T12:01:00Z"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message %q does not mention %q", env.Error.Message, want)
		}
	}

	records := sink.Records()
	if len(records) != 1 {
		t.Fatalf("recorded %d decisions, want 1: a refusal belongs in the audit trail", len(records))
	}
	if got := records[0].Outcome.Status; got != domains.StatusPolicyBlocked {
		t.Errorf("status = %q, want policy_blocked", got)
	}
	if len(records[0].Outcome.Attempts) != 0 {
		t.Error("a blocked request records no attempts; nothing was dispatched")
	}
}

// A monthly limit resets in days. Telling an SDK to retry would have it
// sleep and fail again, so it is told not to.
func TestCompletions_QuotaExceededFarFromReset(t *testing.T) {
	ctrl := gomock.NewController(t)
	r, res, _ := routing(ctrl, []ports.Target{target(unusedProvider(ctrl, "p"), "p", "m")}, nil)

	b := breach(domains.WindowMonth, 5, 1, 5)
	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).
		Return(ports.Reservation{}, &ports.QuotaExceededError{Breach: b})

	h, _ := quotaHandler(r, res, q)
	resp := post(t, h, `{"model":"fast","messages":[]}`, nil)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Should-Retry"); got != "false" {
		t.Errorf("X-Should-Retry = %q, want false", got)
	}
	wantAfter := int64(b.ResetAt().Sub(testNowFn()).Seconds())
	if got := resp.Header.Get("Retry-After"); got != strconv.FormatInt(wantAfter, 10) {
		t.Errorf("Retry-After = %q, want %d", got, wantAfter)
	}
}

// Fail-closed is an outage of ours, not a verdict on the caller: 503,
// never 429, or SDKs would treat it as the tenant's budget.
func TestCompletions_QuotaUnavailableFailsClosedWith503(t *testing.T) {
	ctrl := gomock.NewController(t)
	r, res, _ := routing(ctrl, []ports.Target{target(unusedProvider(ctrl, "p"), "p", "m")}, nil)

	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).
		Return(ports.Reservation{}, fmt.Errorf("%w: dial tcp: refused", ports.ErrQuotaUnavailable))

	h, sink := quotaHandler(r, res, q)
	resp := post(t, h, `{"model":"fast","messages":[]}`, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); strings.Contains(body, "dial tcp") {
		t.Error("infrastructure detail leaked to the client")
	}
	if n := len(sink.Records()); n != 0 {
		t.Errorf("recorded %d decisions for a request never considered", n)
	}
}

// The reservation is sized from the request, and what dispatch actually
// did goes back to reconcile it.
func TestCompletions_ReservesThenReconcilesWithTheOutcome(t *testing.T) {
	ctrl := gomock.NewController(t)
	usage := &domains.TokenUsage{Input: 12, Output: 3}
	p := scriptedProvider(ctrl, "openai", []ports.StreamEvent{
		{Raw: []byte(`{"choices":[{"delta":{"content":"x"}}]}`)},
		{Raw: []byte(`{"choices":[],"usage":{}}`), Usage: usage, UsageOnly: true},
		{Raw: []byte("[DONE]"), Terminal: true},
	})
	r, res, _ := routing(ctrl, []ports.Target{target(p, "openai", "gpt-5")}, nil)

	reservation := ports.Reservation{
		TenantID: uuid.New(),
		Windows:  []domains.Window{{Kind: domains.WindowMinute}},
		Reserved: domains.QuotaUsage{Requests: 1, Tokens: 900},
	}

	var got ports.QuotaRequest
	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req ports.QuotaRequest) (ports.Reservation, error) {
			got = req
			return reservation, nil
		})
	reconciled := make(chan domains.Outcome, 1)
	q.EXPECT().Reconcile(gomock.Any(), reservation, gomock.Any()).Do(
		func(_ context.Context, _ ports.Reservation, o domains.Outcome) { reconciled <- o })

	h, _ := quotaHandler(r, res, q)
	resp := post(t, h,
		`{"model":"fast","messages":[{"role":"user","content":"abcdefgh"}],"max_tokens":64,"n":2,"stream":true}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	readBody(t, resp)

	if got.Tenant != "acme" || !got.At.Equal(testNowFn()) {
		t.Errorf("tenant/at = %q/%v", got.Tenant, got.At)
	}
	if got.Prompt != (domains.PromptSize{TextBytes: 8, Messages: 1}) || got.MaxOutputTokens != 64 || got.Choices != 2 {
		t.Errorf("sizing = %+v max=%d n=%d", got.Prompt, got.MaxOutputTokens, got.Choices)
	}
	if len(got.Targets) != 1 || got.Targets[0] != "openai" {
		t.Errorf("targets = %v, want the ladder's names for pricing", got.Targets)
	}

	select {
	case o := <-reconciled:
		if o.Status != domains.StatusOK || o.Usage != *usage {
			t.Errorf("reconciled with %+v, want the dispatch outcome", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reservation was never reconciled; the tenant would stay locked out")
	}
}

// An exhausted ladder still reconciles: that is what releases the
// reservation for a request nobody served.
func TestCompletions_ExhaustedLadderStillReconciles(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := failingProvider(ctrl, "p", &ports.ProviderError{
		Kind: ports.FailureUpstream, StatusCode: 500, Retryable: true, Message: "boom",
	})
	r, res, _ := routing(ctrl, []ports.Target{target(p, "p", "m")}, nil)

	reservation := ports.Reservation{Windows: []domains.Window{{Kind: domains.WindowDay}}}
	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).Return(reservation, nil)

	reconciled := make(chan domains.Status, 1)
	q.EXPECT().Reconcile(gomock.Any(), reservation, gomock.Any()).Do(
		func(_ context.Context, _ ports.Reservation, o domains.Outcome) { reconciled <- o.Status })

	h, _ := quotaHandler(r, res, q)
	resp := post(t, h, `{"model":"fast","messages":[]}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	select {
	case s := <-reconciled:
		if s != domains.StatusExhausted {
			t.Errorf("status = %q, want exhausted", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an exhausted request was never reconciled")
	}
}

// Gates that refuse for other reasons must not take a quota slot.
func TestCompletions_RefusedRequestsReserveNothing(t *testing.T) {
	ctrl := gomock.NewController(t)
	r, res, _ := routing(ctrl, []ports.Target{target(unusedProvider(ctrl, "p"), "p", "m")},
		errors.New(`unknown model "nope"`))

	q := mocks.NewMockQuotaEnforcer(ctrl)
	q.EXPECT().Reserve(gomock.Any(), gomock.Any()).Times(0)

	h, _ := quotaHandler(r, res, q)
	if resp := post(t, h, `{"model":"nope","messages":[]}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
