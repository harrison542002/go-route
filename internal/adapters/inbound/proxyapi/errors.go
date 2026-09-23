package proxyapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

func statusFor(o domains.Outcome) int {
	if len(o.Attempts) == 0 {
		return http.StatusServiceUnavailable
	}

	last := o.Attempts[len(o.Attempts)-1]
	if last.Failure == nil {
		return http.StatusInternalServerError
	}

	switch last.Failure.StatusCode {
	case 400, 404, 413, 422:
		// Client-caused: the request itself is the problem, so passing
		// the status through lets the caller act on it.
		return last.Failure.StatusCode

	case 401, 403:
		// go-route's own upstream credentials are wrong.
		return http.StatusBadGateway

	case 429:
		// Deliberately NOT 429. The OpenAI SDK auto-retries 429 with
		// backoff, so returning it here turns one exhausted ladder into
		// a retry storm across every target.
		return http.StatusBadGateway

	default:
		return http.StatusBadGateway
	}
}

func writeError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Message: message, Type: errType},
	})
}

func lastMessage(o domains.Outcome) string {
	if len(o.Attempts) == 0 {
		return "no upstream targets were available"
	}
	last := o.Attempts[len(o.Attempts)-1]
	if last.Failure == nil {
		return "upstream failed without a recorded reason"
	}

	switch last.Failure.StatusCode {
	case 400, 404, 413, 422:
		// The client's request is the problem, so the detail is actionable.
		return last.Failure.Message
	default:
		return "all upstream targets failed"
	}
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ports.ErrNoCredentials):
		unauthorized(w, "missing api key: send it as Authorization: Bearer <key>")

	case errors.Is(err, ports.ErrUnknownKey), errors.Is(err, ports.ErrKeyRevoked):
		// One message for both, so the response cannot be used to tell a
		// key that never existed from one that was withdrawn.
		unauthorized(w, "invalid api key")

	case errors.Is(err, ports.ErrTenantDisabled):
		writeError(w, http.StatusForbidden, "tenant is disabled", "permission_error")

	case errors.Is(err, ports.ErrModelNotAllowed):
		writeError(w, http.StatusForbidden, err.Error(), "permission_error")

	default:
		slog.Error("authentication failed", "err", err)
		writeError(w, http.StatusServiceUnavailable,
			"authentication is unavailable", "internal_error")
	}
}

func unauthorized(w http.ResponseWriter, message string) {
	// RFC 9110: a 401 has to say what scheme would satisfy it.
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, message, "authentication_error")
}
