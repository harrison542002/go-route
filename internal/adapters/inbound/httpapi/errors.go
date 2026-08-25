package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/harrison542002/go-route/internal/core/domains"
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

// writeError sends an OpenAI-shaped error envelope.
func writeError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Message: message, Type: errType},
	})
}

// lastMessage returns the most recent failure message from an exhausted
// outcome, for surfacing to the client.
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
