package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	httpmocks "github.com/harrison542002/go-route/internal/adapters/inbound/httpapi/mocks"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
)

// --- builders --------------------------------------------------------

// ref builds a target description. Tests care about the name, since that
// is what reaches decision records; the upstream model matters only where
// a test asserts what the provider was asked for.
func ref(name, upstreamModel string) domains.TargetRef {
	return domains.TargetRef{
		Name:          name,
		Provider:      name,
		UpstreamModel: upstreamModel,
	}
}

func target(p ports.Provider, name, upstreamModel string) ports.Target {
	return ports.Target{
		Provider: p,
		Model:    upstreamModel,
		Ref:      ref(name, upstreamModel),
	}
}

// ladderOf builds a Ladder from targets, deriving the refs from them so
// the router and resolver agree without the test stating things twice.
func ladderOf(targets ...ports.Target) domains.Ladder {
	refs := make([]domains.TargetRef, 0, len(targets))
	for _, t := range targets {
		refs = append(refs, t.Ref)
	}
	return domains.Ladder{
		Targets: refs,
		Reason:  domains.Reason{Kind: domains.ReasonModelAlias, ModelAlias: "fast"},
	}
}

// routing returns a router and resolver pair wired to the same targets,
// plus a pointer to the facts the router received.
//
// The resolver is mocked rather than real because tests supply mock
// providers directly; a real routing.Resolver would need a provider map
// keyed by name, which adds setup without adding coverage here.
func routing(ctrl *gomock.Controller, targets []ports.Target, routeErr error) (
	*httpmocks.MockRouter, *httpmocks.MockResolver, *domains.RequestFacts,
) {
	got := &domains.RequestFacts{}
	ladder := ladderOf(targets...)

	r := httpmocks.NewMockRouter(ctrl)
	r.EXPECT().Route(gomock.Any()).DoAndReturn(
		func(f domains.RequestFacts) (domains.Ladder, error) {
			*got = f
			if routeErr != nil {
				return domains.Ladder{}, routeErr
			}
			return ladder, nil
		}).AnyTimes()

	res := httpmocks.NewMockResolver(ctrl)
	res.EXPECT().Resolve(gomock.Any()).Return(targets, nil).AnyTimes()

	return r, res, got
}

// scriptedProvider yields evs then io.EOF. A fresh reader is built per
// Stream call so a provider can appear more than once in a ladder.
func scriptedProvider(ctrl *gomock.Controller, name string, evs []ports.StreamEvent) *mocks.MockProvider {
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return(name).AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, *ports.ProviderRequest) (ports.StreamReader, error) {
			return scriptedReader(ctrl, evs, io.EOF), nil
		}).AnyTimes()
	return p
}

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

func chunks(texts ...string) []ports.StreamEvent {
	out := make([]ports.StreamEvent, 0, len(texts)+1)
	for _, s := range texts {
		out = append(out, ports.StreamEvent{
			Raw: fmt.Appendf(nil, `{"choices":[{"delta":{"content":%q}}]}`, s),
		})
	}
	return append(out, ports.StreamEvent{Raw: []byte("[DONE]"), Terminal: true})
}

var testNowFn = func() time.Time {
	return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
}

func newHandler(r Router, res Resolver) (*Handler, *recordingSink) {
	s := &recordingSink{}
	return NewHandler(r, res, dispatch.New(testNowFn), s, testNowFn), s
}

type recordingSink struct {
	mu      sync.Mutex
	records []domains.RoutingDecision
}

var _ ports.DecisionSink = (*recordingSink)(nil)

func (s *recordingSink) Record(d domains.RoutingDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, d)
}

func (s *recordingSink) Flush(context.Context) error { return nil }

func (s *recordingSink) Records() []domains.RoutingDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domains.RoutingDecision(nil), s.records...)
}

// --- request helpers -------------------------------------------------

func post(t *testing.T, h *Handler, body string, headers map[string]string) *http.Response {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(h.Completions))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func decodeError(t *testing.T, resp *http.Response) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("response is not an error envelope: %v", err)
	}
	return env
}

// --- happy paths -----------------------------------------------------

func TestCompletions_StreamingSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := scriptedProvider(ctrl, "openai", chunks("Hel", "lo"))
	r, res, _ := routing(ctrl, []ports.Target{target(p, "openai", "upstream-m")}, nil)

	h, _ := newHandler(r, res)
	resp := post(t, h,
		`{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{"x-go-route-feature": "auto-tag"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if id := resp.Header.Get("X-Go-Route-Decision-Id"); !strings.HasPrefix(id, "dec_") {
		t.Errorf("decision ID = %q", id)
	}

	body := readBody(t, resp)
	if !strings.Contains(body, "Hel") || !strings.Contains(body, "[DONE]") {
		t.Errorf("stream not relayed:\n%s", body)
	}
}

// The facts the router receives drive every routing decision, so the
// extraction wiring is worth asserting at the handler level too.
func TestCompletions_PassesFactsToRouter(t *testing.T) {
	ctrl := gomock.NewController(t)
	p := scriptedProvider(ctrl, "openai", chunks("x"))
	r, res, got := routing(ctrl, []ports.Target{target(p, "openai", "m")}, nil)

	h, _ := newHandler(r, res)
	post(t, h,
		`{"model":"fast","messages":[],"stream":true,"stream_options":{"include_usage":true}}`,
		map[string]string{
			"x-go-route-feature":    "auto-tag",
			"x-go-route-data-class": "internal",
			"Authorization":         "Bearer sk-client-secret",
		})

	if got.RequestedModel != "fast" {
		t.Errorf("RequestedModel = %q", got.RequestedModel)
	}
	if !got.Stream || !got.WantsUsage {
		t.Errorf("Stream = %v WantsUsage = %v, want both true", got.Stream, got.WantsUsage)
	}
	if got.Metadata["feature"] != "auto-tag" || got.Metadata["data-class"] != "internal" {
		t.Errorf("metadata = %v", got.Metadata)
	}
	if _, leaked := got.Metadata["authorization"]; leaked {
		t.Error("Authorization leaked into metadata, which lands in the audit log")
	}
	if got.Tenant != domains.DefaultTenant {
		t.Errorf("Tenant = %q", got.Tenant)
	}
	if !got.ReceivedAt.Equal(testNowFn()) {
		t.Errorf("ReceivedAt = %v, want the injected clock value", got.ReceivedAt)
	}
}

// The dispatcher must ask each provider for that target's upstream model,
// not the name the client requested.
func TestCompletions_DispatchesTargetModel(t *testing.T) {
	ctrl := gomock.NewController(t)

	var gotReq ports.ProviderRequest
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return("openai").AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *ports.ProviderRequest) (ports.StreamReader, error) {
			gotReq = *req
			return scriptedReader(ctrl, chunks("x"), io.EOF), nil
		})

	r, res, _ := routing(ctrl, []ports.Target{target(p, "openai", "gpt-5-mini")}, nil)

	h, _ := newHandler(r, res)
	post(t, h, `{"model":"fast","messages":[],"stream":true}`, nil)

	if gotReq.Model != "gpt-5-mini" {
		t.Errorf("Model = %q, want the target's upstream model", gotReq.Model)
	}
	if !gotReq.Stream {
		t.Error("Stream flag not propagated")
	}
	if !strings.Contains(string(gotReq.Body), `"model":"fast"`) {
		t.Error("Body must be the client's original bytes; the adapter rewrites the model")
	}
}

func TestCompletions_NonStreaming(t *testing.T) {
	ctrl := gomock.NewController(t)
	body := `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`

	p := scriptedProvider(ctrl, "openai", []ports.StreamEvent{
		{Raw: []byte(body), Terminal: true},
	})
	r, res, _ := routing(ctrl, []ports.Target{target(p, "openai", "m")}, nil)

	h, _ := newHandler(r, res)
	resp := post(t, h, `{"model":"fast","messages":[]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := readBody(t, resp); got != body {
		t.Errorf("body = %q, want the upstream response with no SSE framing", got)
	}
}

func TestCompletions_FailoverIsInvisibleToTheClient(t *testing.T) {
	ctrl := gomock.NewController(t)
	dead := failingProvider(ctrl, "dead", &ports.ProviderError{
		Kind: ports.FailureUpstream, StatusCode: 503, Retryable: true, Message: "unavailable",
	})
	alive := scriptedProvider(ctrl, "alive", chunks("ok"))

	r, res, _ := routing(ctrl, []ports.Target{
		target(dead, "dead", "a"),
		target(alive, "alive", "b"),
	}, nil)

	h, _ := newHandler(r, res)
	resp := post(t, h, `{"model":"fast","messages":[],"stream":true}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — failover should be transparent", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "ok") {
		t.Errorf("second target's output missing:\n%s", body)
	}
}

// --- request rejection -----------------------------------------------

func TestCompletions_BadRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		routerErr  error
		wantStatus int
		wantIn     string
	}{
		{"malformed JSON", `{"model":`, nil, http.StatusBadRequest, "malformed"},
		{"not JSON at all", `hello`, nil, http.StatusBadRequest, "malformed"},
		{"missing model", `{"messages":[]}`, nil, http.StatusBadRequest, "model"},
		{
			name: "unknown model", body: `{"model":"nope","messages":[]}`,
			routerErr:  errors.New(`unknown model "nope"; available: fast, gpt-5-mini`),
			wantStatus: http.StatusBadRequest, wantIn: "available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			// The provider must never be reached: rejection happens before
			// dispatch.
			p := unusedProvider(ctrl, "p")
			r, res, _ := routing(ctrl, []ports.Target{target(p, "p", "m")}, tt.routerErr)

			h, _ := newHandler(r, res)
			resp := post(t, h, tt.body, nil)

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			env := decodeError(t, resp)
			if !strings.Contains(env.Error.Message, tt.wantIn) {
				t.Errorf("message = %q, want it to mention %q", env.Error.Message, tt.wantIn)
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("type = %q", env.Error.Type)
			}
		})
	}
}

func TestCompletions_BodyTooLarge(t *testing.T) {
	orig := maxBodyBytes
	maxBodyBytes = 1024
	t.Cleanup(func() { maxBodyBytes = orig })

	ctrl := gomock.NewController(t)
	r, res, _ := routing(ctrl, []ports.Target{target(unusedProvider(ctrl, "p"), "p", "m")}, nil)

	big := fmt.Sprintf(`{"model":"fast","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("x", 4096))

	h, _ := newHandler(r, res)
	resp := post(t, h, big, nil)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// Config validation proves every reference resolves at boot, so a
// resolver failure means config and built providers have drifted. The
// client cannot act on it, so it must not leak internals.
func TestCompletions_ResolverFailure(t *testing.T) {
	ctrl := gomock.NewController(t)

	r := httpmocks.NewMockRouter(ctrl)
	r.EXPECT().Route(gomock.Any()).Return(
		domains.Ladder{Targets: []domains.TargetRef{ref("ghost", "m")}}, nil)

	res := httpmocks.NewMockResolver(ctrl)
	res.EXPECT().Resolve(gomock.Any()).Return(nil, errors.New(`no provider "ghost"`))

	h, _ := newHandler(r, res)
	resp := post(t, h, `{"model":"fast","messages":[]}`, nil)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if body := readBody(t, resp); strings.Contains(body, "ghost") {
		t.Error("internal target names leaked to the client")
	}
}

// --- upstream failures -----------------------------------------------

func TestCompletions_ExhaustedLadder(t *testing.T) {
	tests := []struct {
		name       string
		upstream   *ports.ProviderError
		wantStatus int
	}{
		{
			name:       "upstream 500 becomes 502",
			upstream:   &ports.ProviderError{Kind: ports.FailureUpstream, StatusCode: 500, Retryable: true, Message: "boom"},
			wantStatus: http.StatusBadGateway,
		},
		{
			// go-route's own credentials are wrong; telling the client 401
			// sends them checking a key that is not at fault.
			name:       "our auth failure is not the client's",
			upstream:   &ports.ProviderError{Kind: ports.FailureAuth, StatusCode: 401, Retryable: false, Message: "bad key"},
			wantStatus: http.StatusBadGateway,
		},
		{
			// Returning 429 would make client SDKs auto-retry and amplify
			// load against upstreams that are already struggling.
			name:       "rate limit is masked",
			upstream:   &ports.ProviderError{Kind: ports.FailureRateLimit, StatusCode: 429, Retryable: true, Message: "slow down"},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "client-caused 400 passes through",
			upstream:   &ports.ProviderError{Kind: ports.FailureBadRequest, StatusCode: 400, Retryable: false, Message: "bad param"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "connect failure has no upstream status",
			upstream:   &ports.ProviderError{Kind: ports.FailureConnect, Retryable: true, Message: "connection refused"},
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			p := failingProvider(ctrl, "p", tt.upstream)
			r, res, _ := routing(ctrl, []ports.Target{target(p, "p", "m")}, nil)

			h, _ := newHandler(r, res)
			resp := post(t, h, `{"model":"fast","messages":[],"stream":true}`, nil)

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q — a pre-commit failure is a JSON error, not SSE", ct)
			}
			// A second, independent proof that the commit boundary held:
			// this header is only written by ClientStream.Commit.
			if resp.Header.Get("X-Go-Route-Decision-Id") != "" {
				t.Error("decision ID header set on an uncommitted response")
			}
		})
	}
}

// An auth failure must not walk the ladder: every target would fail the
// same way, adding latency for nothing.
func TestCompletions_NonRetryableStopsLadder(t *testing.T) {
	ctrl := gomock.NewController(t)

	bad := failingProvider(ctrl, "bad", &ports.ProviderError{
		Kind: ports.FailureAuth, StatusCode: 401, Retryable: false, Message: "invalid key",
	})
	never := unusedProvider(ctrl, "never")

	r, res, _ := routing(ctrl, []ports.Target{
		target(bad, "bad", "a"),
		target(never, "never", "b"),
	}, nil)

	h, _ := newHandler(r, res)
	resp := post(t, h, `{"model":"fast","messages":[],"stream":true}`, nil)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// Post-commit the status is already 200, so the failure can only be
// delivered in-band and the ladder is over.
func TestCompletions_TruncationAfterCommit(t *testing.T) {
	ctrl := gomock.NewController(t)

	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().Name().Return("flaky").AnyTimes()
	p.EXPECT().Stream(gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, *ports.ProviderRequest) (ports.StreamReader, error) {
			return scriptedReader(ctrl,
				[]ports.StreamEvent{{Raw: []byte(`{"choices":[{"delta":{"content":"a"}}]}`)}},
				io.ErrUnexpectedEOF), nil
		})

	backup := unusedProvider(ctrl, "backup")
	r, res, _ := routing(ctrl, []ports.Target{
		target(p, "flaky", "a"),
		target(backup, "backup", "b"),
	}, nil)

	h, _ := newHandler(r, res)
	resp := post(t, h, `{"model":"fast","messages":[],"stream":true}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — already committed", resp.StatusCode)
	}

	body := readBody(t, resp)
	if !strings.Contains(body, "upstream_error") {
		t.Errorf("in-band error missing:\n%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("a truncated stream must not be terminated with [DONE]")
	}
}
