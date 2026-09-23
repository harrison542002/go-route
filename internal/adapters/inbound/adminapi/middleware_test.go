package adminapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveBody runs one request through the Body middleware and reports what the
// next handler saw.
func serveBody(t *testing.T, method, target, body string) (code int, seen string) {
	t.Helper()
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		seen = string(raw)

		if r.GetBody == nil {
			t.Fatal("GetBody not set")
		}
		again, _ := r.GetBody()
		if raw2, _ := io.ReadAll(again); string(raw2) != seen {
			t.Errorf("GetBody = %q, body = %q", raw2, seen)
		}
	})

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	Body(next).ServeHTTP(rec, req)
	return rec.Code, seen
}

func TestBodyRestoresTheBody(t *testing.T) {
	const body = `{"name":"A"}`
	code, seen := serveBody(t, "PATCH", "/admin/v1/tenants/acme", body)
	if code != http.StatusOK || seen != body {
		t.Fatalf("code %d, next saw %q", code, seen)
	}
}

func TestBodyForcesJSONContentType(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Content-Type")
	})
	req := httptest.NewRequest("PATCH", "/admin/v1/tenants/acme", strings.NewReader(`{"name":"A"}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	Body(next).ServeHTTP(httptest.NewRecorder(), req)
	if seen != "application/json" {
		t.Fatalf("Content-Type = %q", seen)
	}
}

func TestBodyRejectsWhatDecodersWouldSkipPast(t *testing.T) {
	for name, body := range map[string]string{
		"trailing data": `{"name":"A"} {}`,
		"truncated":     `{"name":`,
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := serveBody(t, "PATCH", "/admin/v1/tenants/acme", body)
			if code != http.StatusBadRequest {
				t.Fatalf("code %d, want 400", code)
			}
		})
	}
}

func TestBodyOverTheCapIs413(t *testing.T) {
	body := `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	code, _ := serveBody(t, "PATCH", "/admin/v1/tenants/acme", body)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d, want 413", code)
	}
}

func TestTimeoutPutsTheDeadlineIntoTheContext(t *testing.T) {
	const d = time.Minute
	var deadline time.Time
	var ok bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	})

	start := time.Now()
	Timeout(d)(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/admin/v1/tenants", nil))
	if !ok || deadline.Before(start) || deadline.After(start.Add(d+time.Second)) {
		t.Fatalf("deadline = %v, %v; want about %s from now", deadline, ok, d)
	}
}
