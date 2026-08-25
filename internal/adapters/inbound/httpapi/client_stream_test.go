package httpapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/sse"
	"github.com/harrison542002/go-route/internal/ports"
)

// The dispatcher's entire failover design rests on this: once Commit has
// written the status line, no later WriteHeader can change it.
func TestClientStream_CommitIsIrrevocable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, domains.NewDecisionID(), true)
		if err := s.Commit(); err != nil {
			t.Errorf("commit: %v", err)
		}
		// net/http logs "superfluous WriteHeader" and ignores this.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the commit boundary does not hold", resp.StatusCode)
	}
}

func TestClientStream_StreamingHeaders(t *testing.T) {
	id := domains.NewDecisionID()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, id, true)
		_ = s.Commit()
		_ = s.Send(ports.StreamEvent{Raw: []byte("x")})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	want := map[string]string{
		"Content-Type":           "text/event-stream",
		"Cache-Control":          "no-cache",
		"X-Accel-Buffering":      "no",
		"X-Go-Route-Decision-Id": id.String(),
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// A non-streaming response is ordinary JSON, not SSE.
func TestClientStream_NonStreamingMode(t *testing.T) {
	body := `{"choices":[{"message":{"content":"hello"}}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, domains.NewDecisionID(), false)
		_ = s.Commit()
		_ = s.Send(ports.StreamEvent{Raw: []byte(body), Terminal: true})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "" {
		t.Error("SSE-only header set on a non-streaming response")
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q — no SSE framing on this path", got, body)
	}
}

// Events must reach the client as written, not buffered until the
// handler returns.
func TestClientStream_FlushesIncrementally(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, domains.NewDecisionID(), true)
		_ = s.Commit()
		_ = s.Send(ports.StreamEvent{Raw: []byte(`{"n":1}`)})
		<-release // handler is still running
		_ = s.Send(ports.StreamEvent{Raw: []byte(`{"n":2}`)})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	d := sse.NewDecoder(bufio.NewReader(resp.Body))

	ev, err := d.Next()
	if err != nil {
		t.Fatalf("first event not delivered before the handler finished: %v", err)
	}
	if string(ev.Data) != `{"n":1}` {
		t.Errorf("data = %q", ev.Data)
	}

	close(release)

	if ev, err = d.Next(); err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != `{"n":2}` {
		t.Errorf("data = %q", ev.Data)
	}
}

func TestClientStream_SendErrorEmitsInBand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, domains.NewDecisionID(), true)
		_ = s.Commit()
		_ = s.Send(ports.StreamEvent{Raw: []byte(`{"choices":[]}`)})
		_ = s.SendError(errors.New("upstream died"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — a post-commit error cannot change it", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"upstream_error"`) {
		t.Errorf("error envelope missing:\n%s", body)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Error("a truncated stream must not be terminated with [DONE]")
	}

	// The envelope must still be valid JSON inside the SSE frame, or
	// client SDKs cannot surface it.
	d := sse.NewDecoder(strings.NewReader(string(body)))
	_, _ = d.Next() // the content chunk
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("error event did not decode: %v", err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(ev.Data, &env); err != nil {
		t.Fatalf("error payload is not valid JSON: %v", err)
	}
	if env.Error.Type != "upstream_error" {
		t.Errorf("type = %q", env.Error.Type)
	}
}

func TestClientStream_OrderingGuards(t *testing.T) {
	t.Run("send before commit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := NewClientStream(rec, domains.NewDecisionID(), true)

		if err := s.Send(ports.StreamEvent{Raw: []byte("x")}); err == nil {
			t.Error("Send before Commit should error")
		}
		if rec.Body.Len() != 0 {
			t.Error("nothing should have been written")
		}
	})

	t.Run("send error before commit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := NewClientStream(rec, domains.NewDecisionID(), true)

		if err := s.SendError(errors.New("x")); err == nil {
			t.Error("SendError before Commit should error")
		}
	})

	t.Run("double commit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := NewClientStream(rec, domains.NewDecisionID(), true)

		if err := s.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(); err == nil {
			t.Error("second Commit should error")
		}
	})
}

// Any ResponseWriter wrapper added to this codebase must implement
// Unwrap() http.ResponseWriter, or ResponseController cannot reach the
// underlying writer and streaming silently breaks.
type noUnwrap struct{ http.ResponseWriter }

func TestClientStream_MiddlewareMustUnwrap(t *testing.T) {
	s := NewClientStream(noUnwrap{httptest.NewRecorder()}, domains.NewDecisionID(), true)

	if err := s.Commit(); err != nil {
		t.Logf("Commit failed as expected with a non-unwrapping writer: %v", err)
	}
}
