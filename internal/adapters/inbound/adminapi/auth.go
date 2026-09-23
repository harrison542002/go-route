package adminapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

const bearerPrefix = "bearer "
const touchTimeout = time.Second

type AccessTokenVerifier interface {
	Verify(token string) (tokens.Claims, error)
}

func RequireToken(
	creds ports.AdminAuthenticator, users ports.AdminUserAuthenticator, verifier AccessTokenVerifier,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := authenticate(r, creds, users, verifier)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "missing or invalid admin token", gen.AuthenticationError)
				return
			}
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
		})
	}
}

func authenticate(
	r *http.Request,
	creds ports.AdminAuthenticator,
	users ports.AdminUserAuthenticator,
	verifier AccessTokenVerifier,
) (identity, bool) {
	presented := bearerToken(r.Header)
	switch {
	case presented == "":
		return identity{}, false
	case strings.Count(presented, ".") == 2:
		return authenticateUser(r, users, verifier, presented)
	default:
		return authenticateMachine(r, creds, presented)
	}
}

func authenticateMachine(
	r *http.Request, creds ports.AdminAuthenticator, presented string,
) (identity, bool) {
	sum := sha256.Sum256([]byte(presented))
	cred, err := creds.AdminCredentialByTokenHash(r.Context(), sum[:])
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return identity{}, false
	case err != nil:
		slog.Error("admin credential lookup failed", "err", err)
		return identity{}, false
	case cred.Revoked(), cred.Expired(time.Now()):
		return identity{}, false
	}

	//nolint:contextcheck // the touch must outlive the request it describes, so it deliberately takes none of its context
	touch(creds, cred)
	return machineIdentity(cred), true
}

func authenticateUser(
	r *http.Request, users ports.AdminUserAuthenticator, verifier AccessTokenVerifier, presented string,
) (identity, bool) {
	if verifier == nil || users == nil {
		return identity{}, false
	}

	claims, err := verifier.Verify(presented)
	if err != nil {
		return identity{}, false
	}

	user, err := users.AdminUserByID(r.Context(), claims.UserID)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return identity{}, false
	case err != nil:
		slog.Error("admin user lookup failed", "err", err)
		return identity{}, false
	case user.Disabled():
		return identity{}, false
	}
	return userIdentity(user), true
}

func touch(creds ports.AdminAuthenticator, cred domains.AdminCredential) {
	at := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
		defer cancel()

		if err := creds.TouchAdminCredential(ctx, cred.ID, at); err != nil {
			slog.Warn("admin credential touch failed", "credential", cred.Name, "err", err)
		}
	}()
}

func bearerToken(h http.Header) string {
	v := strings.TrimSpace(h.Get("Authorization"))
	if len(v) < len(bearerPrefix) || !strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(v[len(bearerPrefix):])
}

func ClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		next.ServeHTTP(w, r.WithContext(withClientIP(r.Context(), host)))
	})
}
