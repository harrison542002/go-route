package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var testNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func newRequest(t *testing.T, body string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestExtractFacts(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers map[string]string
		want    domains.RequestFacts
	}{
		{
			name: "minimal non-streaming request",
			body: `{"model":"gpt-5-mini","messages":[{"role":"user","content":"hi"}]}`,
			want: domains.RequestFacts{
				RequestedModel: "gpt-5-mini",
				Stream:         false,
				WantsUsage:     false,
				Metadata:       map[string]string{},
			},
		},
		{
			name: "streaming without stream_options",
			body: `{"model":"m","stream":true,"messages":[]}`,
			want: domains.RequestFacts{
				RequestedModel: "m",
				Stream:         true,
				WantsUsage:     false,
				Metadata:       map[string]string{},
			},
		},
		{
			name: "streaming with include_usage true",
			body: `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`,
			want: domains.RequestFacts{
				RequestedModel: "m",
				Stream:         true,
				WantsUsage:     true,
				Metadata:       map[string]string{},
			},
		},
		{
			name: "stream_options present but include_usage false",
			body: `{"model":"m","stream":true,"stream_options":{"include_usage":false},"messages":[]}`,
			want: domains.RequestFacts{
				RequestedModel: "m",
				Stream:         true,
				WantsUsage:     false,
				Metadata:       map[string]string{},
			},
		},
		{
			name: "metadata headers extracted, prefix stripped and lowercased",
			body: `{"model":"m","messages":[]}`,
			headers: map[string]string{
				"X-Go-Route-Feature":     "auto-tag",
				"x-go-route-DATA-CLASS":  "pii-eu",
				"X-Go-Route-Cost-Center": "eng-42",
			},
			want: domains.RequestFacts{
				RequestedModel: "m",
				Metadata: map[string]string{
					"feature":     "auto-tag",
					"data-class":  "pii-eu",
					"cost-center": "eng-42",
				},
			},
		},
		{
			name: "non-prefixed headers are ignored",
			body: `{"model":"m","messages":[]}`,
			headers: map[string]string{
				"Authorization": "Bearer sk-secret-do-not-log",
				"Content-Type":  "application/json",
				"User-Agent":    "openai-python/1.2.3",
				"X-Request-Id":  "abc123",
			},
			want: domains.RequestFacts{
				RequestedModel: "m",
				Metadata:       map[string]string{},
			},
		},
		{
			name:    "bare prefix with empty key is skipped",
			body:    `{"model":"m","messages":[]}`,
			headers: map[string]string{"X-Go-Route-": "orphan"},
			want: domains.RequestFacts{
				RequestedModel: "m",
				Metadata:       map[string]string{},
			},
		},
		{
			name: "unknown top-level body fields are ignored",
			body: `{"model":"m","messages":[],"temperature":0.7,"tools":[{"type":"function"}],"future_field":{"nested":true}}`,
			want: domains.RequestFacts{
				RequestedModel: "m",
				Metadata:       map[string]string{},
			},
		},
		{
			name: "multimodal content does not break extraction",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`,
			want: domains.RequestFacts{
				RequestedModel: "m",
				Metadata:       map[string]string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, tt.body, tt.headers)

			got, err := ExtractFacts(r, []byte(tt.body), "acme", testNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			want := tt.want
			want.Tenant = "acme"
			want.ReceivedAt = testNow

			if !reflect.DeepEqual(got, want) {
				t.Errorf("\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestExtractFacts_Errors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"model": "m",}`},
		{"not JSON at all", `hello`},
		{"empty body", ``},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"empty model", `{"model":"","messages":[]}`},
		{"body is a JSON array", `[{"model":"m"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, tt.body, nil)

			_, err := ExtractFacts(r, []byte(tt.body), "acme", testNow)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestExtractFacts_TenantComesFromTheCaller(t *testing.T) {
	body := `{"model":"m","messages":[]}`
	r := newRequest(t, body, map[string]string{"X-Go-Route-Tenant": "victim-corp"})

	got, err := ExtractFacts(r, []byte(body), "acme", testNow)
	if err != nil {
		t.Fatal(err)
	}

	if got.Tenant != "acme" {
		t.Errorf("Tenant = %q, want %q -- a header overrode the authenticated tenant", got.Tenant, "acme")
	}
	if v, ok := got.Metadata[tenantKey]; ok {
		t.Errorf("tenant header became metadata key %q = %q", tenantKey, v)
	}
}
