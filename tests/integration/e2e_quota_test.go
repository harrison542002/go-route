//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// The quota path through the real system: config, bootstrap, Redis,
// Postgres, and the HTTP layer. A budget that one request exactly fills
// refuses the second before any upstream is called, with the headers a
// client needs to back off, and the refusal lands in the ledger.
func TestE2E_QuotaExceededIs429(t *testing.T) {
	up := newUpstream(t, streamOK(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		`[DONE]`,
	))

	// A quota is a spend cap, so the budget is what one request costs.
	// At a nanodollar a token, that is the worst case this body is sized
	// at: a 2-byte prompt in one message, plus the default output ceiling.
	estimate := domains.EstimateTokens(domains.PromptSize{TextBytes: 2, Messages: 1}, 0, 1, 4096)
	acme := quotaFor(t, "acme", int64(estimate.Total()))

	srv := boot(t, `
redis: {addr: "`+redisAddr+`", on_unavailable: closed}
providers:
  fake: {type: oaicompat, base_url: `+up.URL+`/v1, api_key: k}
targets:
  fake/m: {provider: fake, model: m}
models:
  chat: [fake/m]
pricing:
  table:
    - effective_from: 2020-01-01
      rates:
        fake/m: {input_per_million: 0.001, output_per_million: 0.001}
`)

	first := post(t, srv, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200 (body %s)", first.StatusCode, readAll(t, first))
	}
	drainSSE(t, first)

	second := post(t, srv, `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 (body %s)", second.StatusCode, readAll(t, second))
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream called %d times; a refused request must not reach it", up.calls.Load())
	}

	retryAfter, err := strconv.Atoi(second.Header.Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Errorf("Retry-After = %q", second.Header.Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(second.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "quota_exceeded" {
		t.Errorf("type = %q", body.Error.Type)
	}
	for _, want := range []string{"spend per period", "$0.000004"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("message %q does not mention %q", body.Error.Message, want)
		}
	}

	// The sink is asynchronous; poll for the ledger row.
	id := second.Header.Get("X-Go-Route-Decision-Id")
	decision, err := domains.ParseDecisionID(id)
	if err != nil {
		t.Fatalf("decision ID %q: %v", id, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		err := pool.QueryRow(context.Background(),
			`SELECT status FROM usage_ledger WHERE id = $1 AND tenant_id = $2`,
			decision.UUID(), acme).Scan(&status)
		if err == nil {
			if status != string(domains.StatusPolicyBlocked) {
				t.Errorf("ledger status = %q, want policy_blocked", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refusal never reached the ledger: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// quotaFor gives a seeded tenant a spend cap for a period around now,
// removed again when the test ends so other tests using the same tenant
// are not capped by it.
func quotaFor(t *testing.T, tenant string, maxCostNanos int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM tenants WHERE external_id = $1`, tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_, err := pool.Exec(ctx, `
		INSERT INTO quotas (tenant_id, window_kind, period_start, period_end, max_cost_nanos)
		VALUES ($1, 'period', $2, $3, $4)`,
		id, start, start.Add(2*time.Hour), maxCostNanos)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM quotas WHERE tenant_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM usage_counters WHERE tenant_id = $1`, id)
	})
	return id
}
