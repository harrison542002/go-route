package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

const since = "2026-09-01T00:00:00Z"

var sinceTime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// newObsServer serves the real chain over a mocked reader.
func newObsServer(t *testing.T) (*httptest.Server, *mocks.MockObservabilityRepository) {
	t.Helper()
	srv, _, obs := newServerWith(t, 10*time.Second)
	return srv, obs
}

// resolves is the tenant lookup every tenant-scoped read starts with.
func resolves(obs *mocks.MockObservabilityRepository) {
	obs.EXPECT().ResolveTenant(gomock.Any(), "acme").
		Return(domains.Tenant("acme"), nil).AnyTimes()
}

func reportRow() domains.ReportRow {
	return domains.ReportRow{
		Key:      "auto-tag",
		Requests: 3,
		Cost:     domains.USD(120_500),
		Unpriced: 1,
		Usage:    domains.TokenUsage{Input: 80, Output: 50, CacheRead: 20, Reasoning: 10},
		Comparisons: map[string]domains.Comparison{
			"openai/gpt-5": {Cost: domains.USD(602_500), Requests: 2},
		},
		OK: 2, Failed: 1,
		P50TTFTMs: 412, P95TTFTMs: 512, P95TotalMs: 1893,
	}
}

func decodeJSON[T any](t *testing.T, body string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %T: %v (%s)", out, err, body)
	}
	return out
}

// --- usage -------------------------------------------------------------

func TestUsageReportsTheGroupedRowsAndTheTotal(t *testing.T) {
	srv, obs := newObsServer(t)
	resolves(obs)

	obs.EXPECT().Aggregate(gomock.Any(), ports.ReportSpec{
		Tenant:  "acme",
		Since:   sinceTime,
		Until:   testNow,
		GroupBy: ports.GroupByMetadata,
		MetaKey: "feature",
		Limit:   10,
	}).Return(domains.Report{
		Spec:            domains.ReportRange{Tenant: "acme", Since: sinceTime, Until: testNow},
		Rows:            []domains.ReportRow{reportRow()},
		Total:           reportRow(),
		TruncatedGroups: 4,
	}, nil)

	resp, body := do(t, srv, call{method: "GET",
		path: "/admin/v1/tenants/acme/usage?since=" + since + "&group_by=metadata&meta_key=feature&limit=10"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	got := decodeJSON[gen.UsageReport](t, body)
	if got.Spec.Tenant != "acme" || got.Spec.GroupBy != gen.UsageGroupByMetadata {
		t.Errorf("spec = %+v", got.Spec)
	}
	if !got.Spec.Until.Equal(testNow) {
		t.Errorf("until = %s, want the server's clock %s", got.Spec.Until, testNow)
	}
	if got.Spec.MetaKey == nil || *got.Spec.MetaKey != "feature" {
		t.Errorf("meta_key = %v", got.Spec.MetaKey)
	}
	if len(got.Rows) != 1 || got.Rows[0].Key != "auto-tag" {
		t.Fatalf("rows = %+v", got.Rows)
	}

	row := got.Rows[0]
	if row.CostNanos != 120_500 || row.Unpriced != 1 || row.Requests != 3 {
		t.Errorf("row = %+v", row)
	}
	if row.Tokens.Input != 80 || row.Tokens.CacheRead != 20 {
		t.Errorf("tokens = %+v", row.Tokens)
	}
	if row.Ok != 2 || row.Failed != 1 {
		t.Errorf("outcomes = %+v", row)
	}
	if row.P50TtftMs != 412 || row.P95TtftMs != 512 || row.P95TotalMs != 1893 {
		t.Errorf("latency = %+v", row)
	}
	if len(row.Comparisons) != 1 || row.Comparisons[0].Target != "openai/gpt-5" ||
		row.Comparisons[0].CostNanos != 602_500 || row.Comparisons[0].Requests != 2 {
		t.Errorf("comparisons = %+v", row.Comparisons)
	}
	if got.TruncatedGroups != 4 {
		t.Errorf("truncated_groups = %d", got.TruncatedGroups)
	}
}

func TestUsageTotalCarriesNoLatency(t *testing.T) {
	srv, obs := newObsServer(t)
	resolves(obs)
	obs.EXPECT().Aggregate(gomock.Any(), gomock.Any()).Return(domains.Report{
		Spec:  domains.ReportRange{Tenant: "acme", Since: sinceTime, Until: testNow},
		Rows:  []domains.ReportRow{reportRow()},
		Total: reportRow(),
	}, nil)

	_, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/acme/usage?since=" + since})

	var envelope struct {
		Total map[string]json.RawMessage `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"p50_ttft_ms", "p95_ttft_ms", "p95_total_ms", "key"} {
		if _, ok := envelope.Total[field]; ok {
			t.Errorf("total carries %s: %s", field, body)
		}
	}
	if _, ok := envelope.Total["cost_nanos"]; !ok {
		t.Errorf("total has no cost: %s", body)
	}
}

func TestUsageRejectsBadParameters(t *testing.T) {
	for name, query := range map[string]string{
		"unknown grouping":         "?since=" + since + "&group_by=tenant",
		"metadata without a key":   "?since=" + since + "&group_by=metadata",
		"meta_key without it":      "?since=" + since + "&group_by=day&meta_key=feature",
		"since after until":        "?since=" + since + "&until=2026-08-01T00:00:00Z",
		"since equal to until":     "?since=" + since + "&until=" + since,
		"limit above the cap":      "?since=" + since + "&limit=5001",
		"no since at all":          "",
		"since that is not a time": "?since=yesterday",
	} {
		t.Run(name, func(t *testing.T) {
			srv, obs := newObsServer(t)
			obs.EXPECT().ResolveTenant(gomock.Any(), gomock.Any()).
				Return(domains.Tenant("acme"), nil).AnyTimes()

			resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/acme/usage" + query})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if errorType(t, body) != "invalid_request_error" {
				t.Errorf("body = %s", body)
			}
		})
	}
}

func TestUsageIs404ForAnUnknownTenant(t *testing.T) {
	srv, obs := newObsServer(t)
	obs.EXPECT().ResolveTenant(gomock.Any(), "nobody").
		Return(domains.Tenant(""), ports.ErrUnknownTenant)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/nobody/usage?since=" + since})
	if resp.StatusCode != http.StatusNotFound || errorType(t, body) != "not_found_error" {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// --- audit -------------------------------------------------------------

func auditEvent(at time.Time, id uuid.UUID) domains.AuditEvent {
	code := 200
	return domains.AuditEvent{
		ID: id, At: at, TenantID: &tenantID,
		Actor: "admin:billing-sync", Action: "quota.upsert",
		Detail: `{"window_kind":"day"}`, StatusCode: &code,
	}
}

func TestAuditReturnsEventsAndACursor(t *testing.T) {
	srv, obs := newObsServer(t)
	resolves(obs)

	id := uuid.MustParse("0192a000-0000-7000-8000-0000000000e1")
	next := ports.AuditCursor{At: sinceTime.Add(time.Hour), ID: id}
	obs.EXPECT().ListAudit(gomock.Any(), ports.AuditQuery{
		Tenant: "acme", Since: sinceTime, Until: testNow, Limit: 2,
	}).Return(ports.AuditPage{
		Events: []domains.AuditEvent{auditEvent(sinceTime.Add(time.Hour), id)},
		Next:   &next,
	}, nil)

	resp, body := do(t, srv, call{method: "GET",
		path: "/admin/v1/tenants/acme/audit?since=" + since + "&limit=2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	got := decodeJSON[gen.AuditPage](t, body)
	if len(got.Events) != 1 || got.Events[0].Action != "quota.upsert" {
		t.Fatalf("events = %+v", got.Events)
	}
	if got.Events[0].Actor != "admin:billing-sync" {
		t.Errorf("actor = %q", got.Events[0].Actor)
	}
	if got.NextCursor.IsNull() {
		t.Fatalf("no cursor: %s", body)
	}
	decoded, err := decodeCursor(got.NextCursor.MustGet())
	if err != nil || decoded.ID != id || !decoded.At.Equal(next.At) {
		t.Errorf("cursor decoded to %+v (%v)", decoded, err)
	}
}

func TestAuditLastPageHasNoCursor(t *testing.T) {
	srv, obs := newObsServer(t)
	resolves(obs)
	obs.EXPECT().ListAudit(gomock.Any(), gomock.Any()).Return(ports.AuditPage{
		Events: []domains.AuditEvent{auditEvent(sinceTime, uuid.New())},
	}, nil)

	_, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/acme/audit?since=" + since})
	if !strings.Contains(body, `"next_cursor":null`) {
		t.Errorf("body = %s", body)
	}
}

func TestAuditPassesTheCursorBackToTheStore(t *testing.T) {
	srv, obs := newObsServer(t)
	resolves(obs)

	after := ports.AuditCursor{At: sinceTime.Add(time.Hour), ID: uuid.New()}
	obs.EXPECT().ListAudit(gomock.Any(), ports.AuditQuery{
		Tenant: "acme", Since: sinceTime, Until: testNow, After: &after,
	}).Return(ports.AuditPage{}, nil)

	resp, body := do(t, srv, call{method: "GET",
		path: "/admin/v1/tenants/acme/audit?since=" + since + "&cursor=" + encodeCursor(after)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestAuditRejectsBadParameters(t *testing.T) {
	for name, query := range map[string]string{
		"garbage cursor":      "?since=" + since + "&cursor=not-a-cursor",
		"truncated cursor":    "?since=" + since + "&cursor=MS4x",
		"limit above the cap": "?since=" + since + "&limit=501",
		"until before since":  "?since=" + since + "&until=2026-08-01T00:00:00Z",
		"no since at all":     "",
	} {
		t.Run(name, func(t *testing.T) {
			srv, obs := newObsServer(t)
			obs.EXPECT().ResolveTenant(gomock.Any(), gomock.Any()).
				Return(domains.Tenant("acme"), nil).AnyTimes()

			resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/acme/audit" + query})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if errorType(t, body) != "invalid_request_error" {
				t.Errorf("body = %s", body)
			}
		})
	}
}

// --- one request -------------------------------------------------------

func testDecision() domains.RoutingDecision {
	return domains.RoutingDecision{
		ID:         domains.NewDecisionID(),
		OccurredAt: sinceTime,
		Tenant:     "acme",
		KeyID:      keyUUID,
		Request: domains.RequestSummary{
			RequestedModel: "chat", Stream: true,
			Metadata: map[string]string{"feature": "auto-tag"},
		},
		Ladder: domains.Ladder{
			Targets: []domains.TargetRef{
				{Name: "openai/gpt-5-mini-eu", Provider: "openai", UpstreamModel: "gpt-5-mini"},
				{Name: "openai/gpt-5-mini", Provider: "openai", UpstreamModel: "gpt-5-mini"},
			},
			Reason: domains.Reason{Kind: domains.ReasonModelAlias, ModelAlias: "chat"},
		},
		Outcome: domains.Outcome{
			Status: domains.StatusOK,
			Attempts: []domains.Attempt{
				{Target: "openai/gpt-5-mini-eu", StartedAt: sinceTime, DurationMs: 31,
					Failure: &domains.AttemptFailure{Kind: "connect", Message: "connection refused", Retryable: true}},
				{Target: "openai/gpt-5-mini", StartedAt: sinceTime, DurationMs: 1893},
			},
			Usage:   domains.TokenUsage{Input: 80, Output: 50},
			TTFTMs:  412,
			TotalMs: 1893,
		},
		Cost: &domains.CostBreakdown{
			Actual:            domains.USD(120_500),
			PriceTableVersion: "2026-08-01",
			Counterfactuals:   []domains.Counterfactual{{Target: "openai/gpt-5", Cost: domains.USD(602_500)}},
		},
	}
}

func TestGetRequestReturnsTheLadderAndTheAttempts(t *testing.T) {
	srv, obs := newObsServer(t)
	d := testDecision()
	obs.EXPECT().Get(gomock.Any(), d.ID).Return(d, nil)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/requests/" + d.ID.String()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	got := decodeJSON[gen.Decision](t, body)
	if got.Id != d.ID.String() || got.Tenant != "acme" || got.Status != gen.Ok {
		t.Errorf("decision = %+v", got)
	}
	if len(got.Ladder.Targets) != 2 || got.Ladder.Targets[0].Name != "openai/gpt-5-mini-eu" {
		t.Errorf("ladder = %+v", got.Ladder)
	}
	if got.Ladder.ReasonKind == nil || *got.Ladder.ReasonKind != "model_alias" {
		t.Errorf("reason = %+v", got.Ladder)
	}
	if len(got.Attempts) != 2 || got.Attempts[0].Failure == nil ||
		got.Attempts[0].Failure.Message != "connection refused" {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	if got.Attempts[1].Failure != nil || got.ChosenTarget == nil ||
		*got.ChosenTarget != "openai/gpt-5-mini" {
		t.Errorf("chosen = %+v", got.ChosenTarget)
	}
	if got.Cost == nil || got.Cost.ActualNanos != 120_500 {
		t.Fatalf("cost = %+v", got.Cost)
	}
	if got.Cost.Counterfactuals == nil || (*got.Cost.Counterfactuals)[0].CostNanos != 602_500 {
		t.Errorf("counterfactuals = %+v", got.Cost.Counterfactuals)
	}
	if got.Timing.TtftMs != 412 || got.Tokens.Input != 80 {
		t.Errorf("timing/tokens = %+v %+v", got.Timing, got.Tokens)
	}
}

func TestGetRequestIs404AndSaysItMayHaveAgedOut(t *testing.T) {
	srv, obs := newObsServer(t)
	id := domains.NewDecisionID()
	obs.EXPECT().Get(gomock.Any(), id).Return(domains.RoutingDecision{}, ports.ErrDecisionNotFound)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/requests/" + id.String()})
	if resp.StatusCode != http.StatusNotFound || errorType(t, body) != "not_found_error" {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(errorBody(t, body).Message, "aged out") {
		t.Errorf("message = %q", errorBody(t, body).Message)
	}
}

func TestGetRequestRejectsAnIdThatIsNotOne(t *testing.T) {
	srv, _ := newObsServer(t)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/requests/not-an-id"})
	if resp.StatusCode != http.StatusBadRequest || errorType(t, body) != "invalid_request_error" {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// --- the role these endpoints exist for --------------------------------

func TestReadonlyCredentialReachesEveryRead(t *testing.T) {
	ctrl := gomock.NewController(t)
	admin := mocks.NewMockAdmin(ctrl)
	obs := mocks.NewMockObservabilityRepository(ctrl)

	obs.EXPECT().ResolveTenant(gomock.Any(), "acme").Return(domains.Tenant("acme"), nil).AnyTimes()
	obs.EXPECT().Aggregate(gomock.Any(), gomock.Any()).Return(domains.Report{
		Spec: domains.ReportRange{Tenant: "acme", Since: sinceTime, Until: testNow},
	}, nil)
	obs.EXPECT().ListAudit(gomock.Any(), gomock.Any()).Return(ports.AuditPage{}, nil)
	d := testDecision()
	obs.EXPECT().Get(gomock.Any(), gomock.Any()).Return(d, nil)

	server, err := NewServer("", NewHandler(admin, nil, obs, func() time.Time { return testNow }),
		newCredentials(testToken, readonlyCredential()), newUsers(testUser), testIssuer(t), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler)
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/admin/v1/tenants/acme/usage?since=" + since,
		"/admin/v1/tenants/acme/audit?since=" + since,
		"/admin/v1/requests/" + d.ID.String(),
	} {
		resp, body := do(t, srv, call{method: "GET", path: path})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, body = %s", path, resp.StatusCode, body)
		}
	}

	resp, _ := do(t, srv, call{method: "DELETE", path: "/admin/v1/tenants/acme"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a readonly credential disabled a tenant: %d", resp.StatusCode)
	}
}
