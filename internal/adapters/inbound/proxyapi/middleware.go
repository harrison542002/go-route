package proxyapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/harrison542002/go-route/internal/ports"
)

const bearerPrefix = "bearer "

type contextKey int

const identityKey contextKey = iota

func Authenticate(auth ports.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, err := auth.Authenticate(r.Context(), bearerToken(r.Header))
			if err != nil {
				writeAuthError(w, err)
				return
			}

			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), identityKey, identity)))
		})
	}
}

func IdentityFrom(ctx context.Context) (ports.Identity, bool) {
	identity, ok := ctx.Value(identityKey).(ports.Identity)
	return identity, ok
}

func bearerToken(h http.Header) string {
	v := strings.TrimSpace(h.Get("Authorization"))
	if len(v) < len(bearerPrefix) || !strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(v[len(bearerPrefix):])
}
