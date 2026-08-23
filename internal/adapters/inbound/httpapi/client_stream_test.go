package httpapi

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harrison542002/go-route/internal/core/sse"
	"github.com/harrison542002/go-route/internal/ports"
)

// The dispatcher's entire failover design rests on this: once Commit has
// written the status line, no later WriteHeader can change it.
func TestClientStream_CommitIsIrrevocable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, "dec_test")
		if err := s.Commit(); err != nil {
			t.Errorf("commit: %v", err)
		}
		// Attempt to retract. net/http logs "superfluous WriteHeader"
		// and ignores it.
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

// Events must reach the client as they are written, not buffered until
// the handler returns.
func TestClientStream_FlushesIncrementally(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, "dec_test")
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

func TestClientStream_Headers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, "dec_abc123")
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
		"X-Go-Route-Decision-Id": "dec_abc123",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestClientStream_SendErrorEmitsInBand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewClientStream(w, "dec_test")
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
		t.Errorf("error envelope missing from body:\n%s", body)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Error("a truncated stream must not be terminated with [DONE]")
	}
}

func TestClientStream_SendBeforeCommitIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewClientStream(rec, "dec_test")

	if err := s.Send(ports.StreamEvent{Raw: []byte("x")}); err == nil {
		t.Error("Send before Commit should error")
	}
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Error("nothing should have been written")
	}
}
