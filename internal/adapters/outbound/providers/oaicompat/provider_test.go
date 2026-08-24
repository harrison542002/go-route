package oaicompat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/harrison542002/go-route/internal/ports"
)

// --- helpers ---------------------------------------------------------

// capture records what the upstream received, so tests can assert on the
// request go-route actually sent.
type capture struct {
	req  *http.Request
	body []byte
}

func newServer(t *testing.T, cap *capture, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.req = r
			cap.body, _ = io.ReadAll(r.Body)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sseHandler(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			_ = rc.Flush()
		}
	}
}

func newProvider(t *testing.T, srv *httptest.Server, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{Name: "test", BaseURL: srv.URL, APIKey: "sk-test"}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func drain(t *testing.T, r ports.StreamReader) []ports.StreamEvent {
	t.Helper()
	var out []ports.StreamEvent
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

// --- request shape ---------------------------------------------------

func TestStream_RequestShape(t *testing.T) {
	var cap capture
	srv := newServer(t, &cap, sseHandler("[DONE]"))
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model:  "upstream-model",
		Body:   []byte(`{"model":"client-asked","messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.7,"future_field":{"x":1}}`),
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rdr.Close()

	t.Run("headers", func(t *testing.T) {
		if ct := cap.req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if a := cap.req.Header.Get("Authorization"); a != "Bearer sk-test" {
			t.Errorf("Authorization = %q", a)
		}
		if ae := cap.req.Header.Get("Accept-Encoding"); ae != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity — SSE must not be compressed", ae)
		}
		if ac := cap.req.Header.Get("Accept"); ac != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", ac)
		}
	})

	t.Run("path and method", func(t *testing.T) {
		if cap.req.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", cap.req.URL.Path)
		}
		if cap.req.Method != http.MethodPost {
			t.Errorf("method = %q", cap.req.Method)
		}
	})

	t.Run("model rewritten to the target", func(t *testing.T) {
		if got := gjson.GetBytes(cap.body, "model").String(); got != "upstream-model" {
			t.Errorf("model = %q, want upstream-model", got)
		}
	})

	t.Run("stream_options injected", func(t *testing.T) {
		if !gjson.GetBytes(cap.body, "stream_options.include_usage").Bool() {
			t.Error("include_usage not injected — streaming requests would have no cost data")
		}
	})

	// The core proxy promise: fields go-route does not model must survive
	// the round trip untouched.
	t.Run("unmodelled fields forwarded verbatim", func(t *testing.T) {
		if got := gjson.GetBytes(cap.body, "temperature").Float(); got != 0.7 {
			t.Errorf("temperature = %v, want 0.7", got)
		}
		if got := gjson.GetBytes(cap.body, "future_field.x").Int(); got != 1 {
			t.Errorf("future_field.x = %v, want 1", got)
		}
	})
}

func TestStream_PreservesExistingStreamOptions(t *testing.T) {
	var cap capture
	srv := newServer(t, &cap, sseHandler("[DONE]"))
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model:  "m",
		Body:   []byte(`{"model":"m","stream":true,"stream_options":{"some_other_flag":true}}`),
		Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if !gjson.GetBytes(cap.body, "stream_options.include_usage").Bool() {
		t.Error("include_usage not injected")
	}
	if !gjson.GetBytes(cap.body, "stream_options.some_other_flag").Bool() {
		t.Error("existing stream_options key was clobbered")
	}
}

func TestStream_DisableStreamOption(t *testing.T) {
	var cap capture
	srv := newServer(t, &cap, sseHandler("[DONE]"))
	p := newProvider(t, srv, func(c *Config) { c.DisableStreamOption = true })

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if gjson.GetBytes(cap.body, "stream_options").Exists() {
		t.Error("stream_options injected despite DisableStreamOption")
	}
}

func TestStream_ExtraHeaders(t *testing.T) {
	var cap capture
	srv := newServer(t, &cap, sseHandler("[DONE]"))
	p := newProvider(t, srv, func(c *Config) {
		c.ExtraHeaders = map[string]string{"api-key": "azure-style"}
	})

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m"}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if got := cap.req.Header.Get("api-key"); got != "azure-style" {
		t.Errorf("api-key = %q", got)
	}
}

// --- response handling -----------------------------------------------

func TestStream_EndToEnd(t *testing.T) {
	srv := newServer(t, nil, sseHandler(
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	))
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	evs := drain(t, rdr)

	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	if evs[0].Usage != nil {
		t.Error("content chunk should carry no usage")
	}
	if evs[2].Usage == nil {
		t.Fatal("usage chunk carried no usage")
	}
	if evs[2].Usage.Input != 8 || evs[2].Usage.CacheRead != 4 {
		t.Errorf("usage = %+v, want Input 8 CacheRead 4", *evs[2].Usage)
	}
	if !evs[2].UsageOnly {
		t.Error("usage chunk not flagged UsageOnly")
	}
	if !evs[3].Terminal {
		t.Error("[DONE] not flagged Terminal")
	}
	if evs[2].Terminal {
		t.Error("usage chunk wrongly flagged Terminal")
	}
}

// Our inability to parse a chunk must never break the client's stream.
func TestStream_UnparseableChunkStillForwarded(t *testing.T) {
	srv := newServer(t, nil, sseHandler(`{"weird":`, `[DONE]`))
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	evs := drain(t, rdr)

	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 — the malformed chunk must still reach the client", len(evs))
	}
	if string(evs[0].Raw) != `{"weird":` {
		t.Errorf("Raw = %q, want the bytes forwarded verbatim", evs[0].Raw)
	}
	if evs[0].Usage != nil {
		t.Error("Usage should be nil for a chunk we could not parse")
	}
}

// A stream cut mid-event must surface as truncation, not a clean end —
// this is what the dispatcher records as StatusTruncated.
func TestStream_TruncatedSurfacesUnexpectedEOF(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"))
		_ = rc.Flush()
		_, _ = w.Write([]byte(`data: {"choices":[{"delta"`)) // cut mid-frame
		_ = rc.Flush()
	})
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if _, err := rdr.Next(); err != nil {
		t.Fatalf("first event: %v", err)
	}
	_, err = rdr.Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestStream_NonStreaming(t *testing.T) {
	body := `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	p := newProvider(t, srv, nil)

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m"}`), Stream: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	ev, err := rdr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Terminal {
		t.Error("the single non-streaming event must be Terminal")
	}
	if ev.Usage == nil || ev.Usage.Input != 5 {
		t.Errorf("usage = %+v", ev.Usage)
	}
	if _, err := rdr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("second Next = %v, want io.EOF", err)
	}
}

// --- errors ----------------------------------------------------------

func TestStream_HTTPErrors(t *testing.T) {
	tests := []struct {
		status    int
		wantKind  ports.FailureKind
		wantRetry bool
	}{
		{401, ports.FailureAuth, false},
		{429, ports.FailureRateLimit, true},
		{500, ports.FailureUpstream, true},
		{400, ports.FailureBadRequest, false},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream says no"}}`))
			})
			p := newProvider(t, srv, nil)

			_, err := p.Stream(context.Background(), &ports.ProviderRequest{
				Model: "m", Body: []byte(`{"model":"m"}`), Stream: true,
			})

			var pe *ports.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *ports.ProviderError", err)
			}
			if pe.Kind != tt.wantKind {
				t.Errorf("kind = %v, want %v", pe.Kind, tt.wantKind)
			}
			if pe.Retryable != tt.wantRetry {
				t.Errorf("retryable = %v, want %v", pe.Retryable, tt.wantRetry)
			}
			if pe.Message != "upstream says no" {
				t.Errorf("message = %q", pe.Message)
			}
		})
	}
}

// Never reached the provider: always safe for the dispatcher to try the
// next target.
func TestStream_ConnectFailureIsRetryable(t *testing.T) {
	p := newProvider(t, httptest.NewServer(nil), func(c *Config) {
		c.BaseURL = "http://127.0.0.1:1" // nothing listens here
	})

	_, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m"}`), Stream: true,
	})

	var pe *ports.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ports.ProviderError", err)
	}
	if pe.Kind != ports.FailureConnect || !pe.Retryable {
		t.Errorf("kind = %v retryable = %v, want connect/true", pe.Kind, pe.Retryable)
	}
}

func TestStream_ContextCancellation(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for i := 0; i < 100; i++ {
			if _, err := w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n")); err != nil {
				return
			}
			_ = rc.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})
	p := newProvider(t, srv, nil)

	ctx, cancel := context.WithCancel(context.Background())
	rdr, err := p.Stream(ctx, &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if _, err := rdr.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()

	// The read must fail promptly rather than draining all 100 chunks.
	deadline := time.After(2 * time.Second)
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := rdr.Next(); err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case err := <-done:
		if errors.Is(err, io.EOF) {
			t.Error("stream ended cleanly; cancellation was not propagated")
		}
	case <-deadline:
		t.Fatal("read did not stop after cancellation")
	}
}

// --- config ----------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Error("missing Name should error")
	}
	if _, err := New(Config{Name: "x"}); err == nil {
		t.Error("missing BaseURL should error")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	var cap capture
	srv := newServer(t, &cap, sseHandler("[DONE]"))

	p, err := New(Config{Name: "t", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}

	rdr, err := p.Stream(context.Background(), &ports.ProviderRequest{
		Model: "m", Body: []byte(`{"model":"m"}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	if cap.req.URL.Path != "/chat/completions" {
		t.Errorf("path = %q — double slash from an unrimmed base URL", cap.req.URL.Path)
	}
}

// These two zeros are load-bearing and look like mistakes. See
// docs/adr/0004-streaming-timeouts.md.
func TestDefaultClient_TimeoutIsZero(t *testing.T) {
	p, err := New(Config{Name: "t", BaseURL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	if p.client.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 — a non-zero value truncates long generations", p.client.Timeout)
	}

	tr, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("default transport is not *http.Transport")
	}
	if !tr.DisableCompression {
		t.Error("compression enabled — gzip buffering defeats streaming")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout unset — a hung upstream would never fail over")
	}
}
