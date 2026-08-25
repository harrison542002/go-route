package oaicompat

import (
	"encoding/json"

	"github.com/harrison542002/go-route/internal/ports"
)

const maxErrorBodyBytes = 8 << 10

// errorResponse is the OpenAI error envelope. Most compatible providers
// emit it; those that do not fall back to the raw body as the message.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// classify maps an upstream HTTP status onto a failure kind and a
// retry decision. The retry decision is what drives the ladder, so the
// reasoning behind each case is recorded here rather than inferred.
func classify(provider string, status int, body []byte) *ports.ProviderError {
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}

	pe := &ports.ProviderError{
		Provider:   provider,
		StatusCode: status,
		Message:    extractMessage(body),
	}

	switch {
	case status == 401, status == 403:
		// Bad credentials for this target.
		pe.Kind, pe.Retryable = ports.FailureAuth, false

	case status == 400, status == 422:
		// The request itself is malformed. Every target will reject it
		// identically, so retrying only adds latency.
		pe.Kind, pe.Retryable = ports.FailureBadRequest, false

	case status == 404:
		// Usually "model not found": a config error for this target, but
		// another target may serve the model perfectly well.
		pe.Kind, pe.Retryable = ports.FailureBadRequest, true

	case status == 413:
		// Payload too large. A target with a bigger limit may accept it.
		pe.Kind, pe.Retryable = ports.FailureBadRequest, true

	case status == 408, status == 504:
		pe.Kind, pe.Retryable = ports.FailureTimeout, true

	case status == 429:
		pe.Kind, pe.Retryable = ports.FailureRateLimit, true

	case status >= 500:
		pe.Kind, pe.Retryable = ports.FailureUpstream, true

	default:
		pe.Kind, pe.Retryable = ports.FailureUnknown, true
	}

	return pe
}

func extractMessage(body []byte) string {
	var er errorResponse
	if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
		return er.Error.Message
	}
	if len(body) == 0 {
		return "upstream returned an empty error body"
	}
	return string(body)
}
