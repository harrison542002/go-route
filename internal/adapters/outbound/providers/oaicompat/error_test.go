package oaicompat

import (
	"strings"
	"testing"

	"github.com/harrison542002/go-route/internal/ports"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantKind  ports.FailureKind
		wantRetry bool
	}{
		{"unauthorized", 401, ports.FailureAuth, false},
		{"forbidden", 403, ports.FailureAuth, false},

		// Malformed request: every target rejects it identically, so
		// retrying only adds latency.
		{"bad request", 400, ports.FailureBadRequest, false},
		{"unprocessable", 422, ports.FailureBadRequest, false},

		// Model not found is a config error for THIS target; another
		// target may serve the model.
		{"not found", 404, ports.FailureBadRequest, true},
		{"payload too large", 413, ports.FailureBadRequest, true},

		{"request timeout", 408, ports.FailureTimeout, true},
		{"gateway timeout", 504, ports.FailureTimeout, true},
		{"rate limited", 429, ports.FailureRateLimit, true},

		{"internal error", 500, ports.FailureUpstream, true},
		{"bad gateway", 502, ports.FailureUpstream, true},
		{"unavailable", 503, ports.FailureUpstream, true},

		{"unmapped 4xx", 418, ports.FailureUnknown, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classify("openai", tt.status, []byte(`{"error":{"message":"x"}}`))

			if pe.Kind != tt.wantKind {
				t.Errorf("kind = %v, want %v", pe.Kind, tt.wantKind)
			}
			if pe.Retryable != tt.wantRetry {
				t.Errorf("retryable = %v, want %v", pe.Retryable, tt.wantRetry)
			}
			if pe.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", pe.StatusCode, tt.status)
			}
			if pe.Provider != "openai" {
				t.Errorf("provider = %q", pe.Provider)
			}
		})
	}
}

func TestExtractMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai envelope",
			body: `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`,
			want: "Incorrect API key provided",
		},
		{
			name: "non-JSON falls back to raw text",
			body: `<html><body>502 Bad Gateway</body></html>`,
			want: `<html><body>502 Bad Gateway</body></html>`,
		},
		{
			name: "JSON without an error envelope falls back",
			body: `{"detail":"something went wrong"}`,
			want: `{"detail":"something went wrong"}`,
		},
		{
			name: "empty body gets a placeholder",
			body: ``,
			want: "upstream returned an empty error body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractMessage([]byte(tt.body)); got != tt.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// Error bodies land in the decision log. An unbounded HTML error page
// from a misconfigured gateway must not be stored in full.
func TestClassify_TruncatesOversizedBody(t *testing.T) {
	huge := strings.Repeat("x", maxErrorBodyBytes*2)

	pe := classify("openai", 500, []byte(huge))

	if len(pe.Message) > maxErrorBodyBytes {
		t.Errorf("message length = %d, want <= %d", len(pe.Message), maxErrorBodyBytes)
	}
}

// The dispatcher classifies failures with errors.As, so ProviderError
// must satisfy the error interface as a pointer.
func TestProviderError_IsAnError(t *testing.T) {
	var err error = classify("openai", 429, []byte(`{"error":{"message":"slow down"}}`))

	if err.Error() != "slow down" {
		t.Errorf("Error() = %q", err.Error())
	}
}
