// Package e2e wires the real components together and drives them over a
// real socket against a fake upstream. It is the only place where a
// config file, the router, the resolver, the dispatcher, the adapter, the
// sink, and the HTTP layer are exercised as one system.
//
// It builds through bootstrap.Build — the same entry point main.go uses —
// so the wiring under test cannot drift from the wiring that ships.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/httpapi"
	"github.com/harrison542002/go-route/internal/bootstrap"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/sse"
)

// upstream is a fake OpenAI-compatible provider. It records what it
// received so tests can assert on the request go-route actually sent.
type upstream struct {
	*httptest.Server

	mu        chan struct{} // guards gotBody/gotHeader; see record()
	gotBody   []byte
	gotHeader http.Header
	calls     atomic.Int32
}

func newUpstream(t *testing.T, respond http.HandlerFunc) *upstream {
	t.Helper()

	u := &upstream{mu: make(chan struct{}, 1)}
	u.mu <- struct{}{}

	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		u.record(body, r.Header.Clone())
		respond(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// record serialises writes. Handlers run on their own goroutines, and
// -race will flag a plain assignment even in tests that make one request.
func (u *upstream) record(body []byte, h http.Header) {
	<-u.mu
	u.gotBody, u.gotHeader = body, h
	u.mu <- struct{}{}
}

func (u *upstream) received() ([]byte, http.Header) {
	<-u.mu
	defer func() { u.mu <- struct{}{} }()
	return u.gotBody, u.gotHeader
}

func streamOK(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			_ = rc.Flush()
			time.Sleep(2 * time.Millisecond) // force separate flushes
		}
	}
}

func fail(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream says no"}}`))
	}
}

// boot writes a config and builds the system through bootstrap.Build.
func boot(t *testing.T, yaml string) (*httptest.Server, *bootstrap.App) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	a, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		if a.Close != nil {
			_ = a.Close()
		}
	})

	srv := httptest.NewServer(httpapi.NewServer("", a.Handler).Handler)
	t.Cleanup(srv.Close)
	return srv, a
}

// records flushes the sink and returns what was written.
//
// Record is asynchronous by design, so reading without flushing is racy.
// Flush closes the sink permanently: call it once, after the last request
// in a test.
func records(t *testing.T, a *bootstrap.App) []domains.RoutingDecision {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if a.Memory == nil {
		t.Fatal("this test needs sink: {type: memory} in its config")
	}
	return a.Memory.Records()
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func drainSSE(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out []string
	d := sse.NewDecoder(bufio.NewReader(resp.Body))
	for {
		ev, err := d.Next()
		if err != nil {
			return out
		}
		out = append(out, string(ev.Data))
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// --- tests -----------------------------------------------------------

func TestE2E_StreamingRoundTrip(t *testing.T) {
	up := newUpstream(t, streamOK(
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8}}`,
		`[DONE]`,
	))

	srv, _ := boot(t, `
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: sk-test}
targets:
  fake/m: {provider: fake, model: upstream-model}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if id := resp.Header.Get("X-Go-Route-Decision-Id"); !strings.HasPrefix(id, "dec_") {
		t.Errorf("decision ID = %q", id)
	}

	// The client must NOT see the usage chunk it never asked for.
	events := drainSSE(t, resp)
	if len(events) != 3 {
		t.Fatalf("client saw %d events, want 3 (the injected usage chunk must be stripped):\n%v",
			len(events), events)
	}
	if events[2] != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", events[2])
	}

	// Read the upstream's view only after the response is fully drained,
	// so the handler goroutine has finished writing to it.
	body, hdr := up.received()

	var sent struct{ Model string }
	_ = json.Unmarshal(body, &sent)
	if sent.Model != "upstream-model" {
		t.Errorf("upstream received model %q, want upstream-model", sent.Model)
	}
	if !strings.Contains(string(body), `"include_usage":true`) {
		t.Error("stream_options not injected; streaming requests would have no cost data")
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("upstream auth = %q", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("upstream Content-Type = %q", got)
	}
}

func TestE2E_NonStreamingRoundTrip(t *testing.T) {
	respBody := `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	})

	srv, _ := boot(t, `
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[]}`)

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := readAll(t, resp); got != respBody {
		t.Errorf("body = %q, want the upstream response with no SSE framing", got)
	}

	body, _ := up.received()
	if strings.Contains(string(body), "stream_options") {
		t.Error("stream_options injected into a non-streaming request")
	}
}

// The whole failover ladder, resolved from config, over real sockets.
func TestE2E_FailoverAcrossProviders(t *testing.T) {
	dead := newUpstream(t, fail(http.StatusServiceUnavailable))
	alive := newUpstream(t, streamOK(`{"choices":[{"delta":{"content":"ok"}}]}`, `[DONE]`))

	srv, _ := boot(t, `
providers:
  dead:  {type: oaicompat, base_url: `+dead.URL+`/v1, api_key: k}
  alive: {type: oaicompat, base_url: `+alive.URL+`/v1, api_key: k}
targets:
  dead/m:  {provider: dead,  model: a}
  alive/m: {provider: alive, model: b}
models:
  chat: [dead/m, alive/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — failover should be transparent", resp.StatusCode)
	}
	if body := readAll(t, resp); !strings.Contains(body, "ok") {
		t.Errorf("second target's output missing:\n%s", body)
	}
	if dead.calls.Load() != 1 || alive.calls.Load() != 1 {
		t.Errorf("calls: dead=%d alive=%d, want 1 each", dead.calls.Load(), alive.calls.Load())
	}
}

// An auth failure must stop the ladder rather than burning every target.
func TestE2E_NonRetryableStopsLadder(t *testing.T) {
	bad := newUpstream(t, fail(http.StatusUnauthorized))
	never := newUpstream(t, streamOK(`[DONE]`))

	srv, _ := boot(t, `
providers:
  bad:   {type: oaicompat, base_url: `+bad.URL+`/v1, api_key: wrong}
  never: {type: oaicompat, base_url: `+never.URL+`/v1, api_key: k}
targets:
  bad/m:   {provider: bad,   model: a}
  never/m: {provider: never, model: b}
models:
  chat: [bad/m, never/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 — our credentials, not the client's", resp.StatusCode)
	}
	if body := readAll(t, resp); strings.Contains(body, "upstream says no") {
		t.Error("upstream error text leaked to the client")
	}
	if never.calls.Load() != 0 {
		t.Error("ladder continued past a non-retryable auth failure")
	}
}

// Tokens must reach the client as generated, not batched at the end.
func TestE2E_TokensArriveIncrementally(t *testing.T) {
	release := make(chan struct{})

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"first"}}]}` + "\n\n"))
		_ = rc.Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		_ = rc.Flush()
	})

	srv, _ := boot(t, `
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)

	d := sse.NewDecoder(bufio.NewReader(resp.Body))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("first event did not arrive while upstream was still generating: %v", err)
	}
	if !strings.Contains(string(ev.Data), "first") {
		t.Errorf("data = %q", ev.Data)
	}
	close(release)
}

// A stream cut mid-frame is truncation, not a clean end. Past the commit
// boundary the status is already 200, so the failure is in-band.
func TestE2E_UpstreamTruncation(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"))
		_ = rc.Flush()
		_, _ = w.Write([]byte(`data: {"choices":[{"delta"`)) // cut mid-frame
		_ = rc.Flush()
	})

	srv, a := boot(t, `
sink: {type: memory}
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — already committed", resp.StatusCode)
	}

	body := readAll(t, resp)
	if !strings.Contains(body, "upstream_error") {
		t.Errorf("in-band error missing:\n%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("a truncated stream must not be terminated with [DONE]")
	}

	// A truncated stream is exactly the case an audit trail must capture.
	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Outcome.Status != domains.StatusTruncated {
		t.Errorf("status = %q, want truncated", recs[0].Outcome.Status)
	}
}

// Config validation must reject dangling references at load, not on the
// first live request.
func TestE2E_ConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "model references an unknown target",
			yaml: `
providers:
  p: {type: oaicompat, base_url: http://x/v1, api_key: k}
targets:
  p/m: {provider: p, model: m}
models:
  chat: [p/ghost]
`,
			want: "unknown target",
		},
		{
			name: "target references an unknown provider",
			yaml: `
providers:
  p: {type: oaicompat, base_url: http://x/v1, api_key: k}
targets:
  q/m: {provider: ghost, model: m}
models:
  chat: [q/m]
`,
			want: "unknown provider",
		},
		{
			name: "unset environment variable",
			yaml: `
providers:
  p: {type: oaicompat, base_url: http://x/v1, api_key: "${GO_ROUTE_DEFINITELY_UNSET}"}
targets:
  p/m: {provider: p, model: m}
models:
  chat: [p/m]
`,
			want: "unset environment",
		},
		{
			name: "unknown sink type",
			yaml: `
sink: {type: nonsense}
providers:
  p: {type: oaicompat, base_url: http://x/v1, api_key: k}
targets:
  p/m: {provider: p, model: m}
models:
  chat: [p/m]
`,
			want: "sink",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := config.Load(path)
			if err == nil {
				t.Fatal("config loaded despite being invalid")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// The client must not be able to reach a model that is not configured.
func TestE2E_UnknownModelRejected(t *testing.T) {
	up := newUpstream(t, streamOK(`[DONE]`))

	srv, _ := boot(t, `
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"not-configured","messages":[]}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if body := readAll(t, resp); !strings.Contains(body, "chat") {
		t.Errorf("error should list configured models:\n%s", body)
	}
	if up.calls.Load() != 0 {
		t.Error("an unknown model reached an upstream")
	}
}

func TestE2E_DecisionRecorded(t *testing.T) {
	up := newUpstream(t, streamOK(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	))

	srv, a := boot(t, `
sink: {type: memory}
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: upstream-model}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
	_ = readAll(t, resp) // the record is written when the handler returns

	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]

	if r.ID.IsZero() {
		t.Error("decision has no ID")
	}
	if r.Outcome.Status != domains.StatusOK {
		t.Errorf("status = %q", r.Outcome.Status)
	}
	if r.Outcome.ChosenTarget() != "fake/m" {
		t.Errorf("chosen = %q, want the stable config name", r.Outcome.ChosenTarget())
	}
	if r.Ladder.Reason.ModelAlias != "chat" {
		t.Errorf("reason lost: %+v", r.Ladder.Reason)
	}
	if r.Outcome.Usage.Input != 8 || r.Outcome.Usage.CacheRead != 4 {
		t.Errorf("usage = %+v, want Input 8 CacheRead 4", r.Outcome.Usage)
	}
	if r.Request.Metadata == nil {
		t.Error("metadata map is nil; reports index into it")
	}
}

// Failures are the most interesting rows in the audit log; a record must
// exist even when nothing was served.
func TestE2E_FailureRecorded(t *testing.T) {
	dead := newUpstream(t, fail(http.StatusServiceUnavailable))

	srv, a := boot(t, `
sink: {type: memory}
providers:
  dead: {type: oaicompat, base_url: `+dead.URL+`/v1, api_key: k}
targets:
  dead/m: {provider: dead, model: m}
models:
  chat: [dead/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
	_ = readAll(t, resp)

	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 — failures must still be recorded", len(recs))
	}
	if recs[0].Outcome.Status != domains.StatusExhausted {
		t.Errorf("status = %q, want exhausted", recs[0].Outcome.Status)
	}
	if len(recs[0].Outcome.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(recs[0].Outcome.Attempts))
	}
	if recs[0].Outcome.ChosenTarget() != "" {
		t.Error("an exhausted outcome must not name a chosen target")
	}
}

// The whole thesis in one test: a config file with rates produces an
// audit record carrying what the request cost, under which price table,
// and what the alternative would have cost.
func TestE2E_DecisionIsPriced(t *testing.T) {
	up := newUpstream(t, streamOK(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":20}}}`,
		`[DONE]`,
	))

	srv, a := boot(t, `
sink: {type: memory}
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/mini:     {provider: fake, model: mini-model}
  fake/flagship: {provider: fake, model: flagship-model}
models:
  chat: [fake/mini]
pricing:
  compare_against: [fake/flagship]
  table:
    - effective_from: 2026-08-01
      rates:
        fake/mini:
          input_per_million: 0.25
          output_per_million: 2.00
          cache_read_per_million: 0.025
        fake/flagship:
          input_per_million: 1.25
          output_per_million: 10.00
          cache_read_per_million: 0.125
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
	_ = readAll(t, resp)

	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]

	if r.Cost == nil {
		t.Fatal("decision was not priced; check that the pricing sink layer is wired")
	}

	// 80 fresh input at $0.25/M, 20 cached at $0.025/M, 50 output at $2/M.
	// 80×250 + 20×25 + 50×2000 = 20000 + 500 + 100000 nanos.
	if want := domains.USD(120_500); r.Cost.Actual != want {
		t.Errorf("actual = %d nanos (%v), want %d (%v)",
			r.Cost.Actual, r.Cost.Actual, want, want)
	}

	if r.Cost.PriceTableVersion != "2026-08-01" {
		t.Errorf("price table = %q; without it a report could not be reproduced",
			r.Cost.PriceTableVersion)
	}

	delta, ok := r.Cost.Delta("fake/flagship")
	if !ok {
		t.Fatalf("no flagship counterfactual: %+v", r.Cost.Counterfactuals)
	}
	if delta >= 0 {
		t.Errorf("delta = %v, want negative — the cheaper target should cost less", delta)
	}
	if len(r.Cost.UnpricedComparisons) != 0 {
		t.Errorf("unpriced comparisons = %v, want none", r.Cost.UnpricedComparisons)
	}

	t.Logf("actual %v, flagship would have been %v, saved %v",
		r.Cost.Actual, r.Cost.Actual-delta, -delta)
}

// Cached input is billed roughly ten times below fresh input.
func TestE2E_CachedTokensAreBilledSeparately(t *testing.T) {
	pricingYAML := `
sink: {type: memory}
providers:
  fake: {type: oaicompat, base_url: %s/v1, api_key: k}
targets:
  fake/mini: {provider: fake, model: mini-model}
models:
  chat: [fake/mini]
pricing:
  table:
    - effective_from: 2026-08-01
      rates:
        fake/mini:
          input_per_million: 0.25
          output_per_million: 2.00
          cache_read_per_million: 0.025
`

	costFor := func(t *testing.T, usageChunk string) domains.USD {
		t.Helper()
		up := newUpstream(t, streamOK(usageChunk, `[DONE]`))
		srv, a := boot(t, fmt.Sprintf(pricingYAML, up.URL))

		resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
		_ = readAll(t, resp)

		recs := records(t, a)
		if len(recs) != 1 || recs[0].Cost == nil {
			t.Fatalf("no priced record: %+v", recs)
		}
		return recs[0].Cost.Actual
	}

	fresh := costFor(t, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10}}`)
	cached := costFor(t, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":900}}}`)

	if cached >= fresh {
		t.Fatalf("cached (%v) is not cheaper than fresh (%v); "+
			"the adapter is probably not splitting cached_tokens out of prompt_tokens",
			cached, fresh)
	}
	t.Logf("fresh %v, 90%% cached %v", fresh, cached)
}

// An exhausted ladder consumed nothing, so there is no cost. Nil is
// distinct from zero: "we don't know" and "it was free" must not sum
// together in a report.
func TestE2E_FailedRequestIsUnpriced(t *testing.T) {
	dead := newUpstream(t, fail(http.StatusServiceUnavailable))

	srv, a := boot(t, `
sink: {type: memory}
providers:
  dead: {type: oaicompat, base_url: `+dead.URL+`/v1, api_key: k}
targets:
  dead/m: {provider: dead, model: m}
models:
  chat: [dead/m]
pricing:
  table:
    - effective_from: 2026-08-01
      rates:
        dead/m: {input_per_million: 0.25, output_per_million: 2.00}
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
	_ = readAll(t, resp)

	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Cost != nil {
		t.Errorf("cost = %+v, want nil — nothing was served", recs[0].Cost)
	}
}

// A deployment without a price table is valid: every record simply
// carries a nil cost. The proxy must not fail or invent zeros.
func TestE2E_NoPricingConfigured(t *testing.T) {
	up := newUpstream(t, streamOK(
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	))

	srv, a := boot(t, `
sink: {type: memory}
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
`)

	resp := post(t, srv, `{"model":"chat","messages":[],"stream":true}`)
	_ = readAll(t, resp)

	recs := records(t, a)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Cost != nil {
		t.Errorf("cost = %+v, want nil with no price table", recs[0].Cost)
	}
	if recs[0].Outcome.Usage.Input == 0 {
		t.Error("usage lost; pricing being absent must not affect metering")
	}
}
