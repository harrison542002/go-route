// Package adminapi is the internal admin API: an HTTP surface over tenants,
// their API keys and their quotas, served on its own listener and guarded by a
// per-caller bearer token. The contract is schemas/admin/openapi.yaml, from
// which the types, routing and decoding in gen/ are generated.
package adminapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// writeGrace is how much longer than a request's deadline the server lets its
// response take to write: a request that times out still has to be told so,
// after the deadline has passed.
const writeGrace = 5 * time.Second

func NewServer(
	addr string,
	h *Handler,
	creds ports.AdminAuthenticator,
	users ports.AdminUserAuthenticator,
	verifier AccessTokenVerifier,
	requestTimeout time.Duration,
) (*http.Server, error) {
	if requestTimeout <= 0 {
		return nil, errors.New("adminapi: request timeout must be positive")
	}
	if creds == nil {
		return nil, errors.New("adminapi: a credential store is required")
	}
	if users == nil || verifier == nil {
		return nil, errors.New("adminapi: a user store and an access token verifier are required")
	}
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("adminapi: load embedded spec: %w", err)
	}

	strict := gen.NewStrictHandlerWithOptions(h, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: fail,
	})
	api := gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		ErrorHandlerFunc: badRequest,
	})

	admin := api
	for _, mw := range []func(http.Handler) http.Handler{
		Validate(spec),
		Body,
		Timeout(requestTimeout),
		Guard(RequireToken(creds, users, verifier), RequirePermission),
		ClientIP,
	} {
		admin = mw(admin)
	}

	root := http.NewServeMux()
	root.Handle("/admin/", admin)
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      requestTimeout + writeGrace,
		IdleTimeout:       120 * time.Second,
	}, nil
}
