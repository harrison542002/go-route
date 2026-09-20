package httpapi

import (
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/ports"
)

func NewServer(addr string, h *Handler, auth ports.Authenticator) *http.Server {
	authenticated := Authenticate(auth)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", authenticated(http.HandlerFunc(h.Completions)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,

		// MUST be zero. WriteTimeout is an absolute deadline from the
		// start of the response, not an idle timeout, so any non-zero
		// value cuts every generation longer than it (Mid-stream, past
		// the commit boundary, with no way to fail over.)
		// See docs/adr/0004-streaming-timeouts.md.
		WriteTimeout: 0,
	}
}
