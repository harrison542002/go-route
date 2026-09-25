package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const testToken = "0123456789abcdef0123456789abcdef"

var (
	tenantID = uuid.MustParse("0192a000-0000-7000-8000-000000000001")
	keyUUID  = uuid.MustParse("0192a000-0000-7000-8000-0000000000aa")
	created  = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
)

func acme() domains.TenantAccount {
	return domains.TenantAccount{
		ID: tenantID, ExternalID: "acme", Name: "Acme",
		Metadata: []byte(`{"plan":"pro"}`), CreatedAt: created,
	}
}

func newServer(t *testing.T) (*httptest.Server, *mocks.MockAdmin) {
	t.Helper()
	return newServerWithTimeout(t, 10*time.Second)
}

// newServerWithTimeout serves the real handler chain over a mocked use case.
func newServerWithTimeout(t *testing.T, timeout time.Duration) (*httptest.Server, *mocks.MockAdmin) {
	t.Helper()
	srv, admin, _ := newServerWith(t, timeout)
	return srv, admin
}

// testNow is the clock the handlers default an open-ended period to.
var testNow = time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)

func newServerWith(
	t *testing.T, timeout time.Duration,
) (*httptest.Server, *mocks.MockAdmin, *mocks.MockObservabilityRepository) {
	t.Helper()
	ctrl := gomock.NewController(t)
	admin := mocks.NewMockAdmin(ctrl)
	obs := mocks.NewMockObservabilityRepository(ctrl)

	server, err := NewServer("",
		NewHandler(admin, nil, obs, func() time.Time { return testNow }),
		newCredentials(testToken, testCredential), newUsers(testUser), testIssuer(t), timeout)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler)
	t.Cleanup(srv.Close)
	return srv, admin, obs
}

type call struct {
	method, path, body string
	header             map[string]string
	noAuth             bool
}

func do(t *testing.T, srv *httptest.Server, c call) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	if !c.noAuth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func errorType(t *testing.T, body string) string {
	t.Helper()
	return string(errorBody(t, body).Type)
}

func errorBody(t *testing.T, body string) gen.ErrorBody {
	t.Helper()
	var env gen.Error
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Type == "" {
		t.Fatalf("not an error envelope: %s", body)
	}
	return env.Error
}

// --- authentication ----------------------------------------------------

func TestRejectsMissingAndWrongTokens(t *testing.T) {
	srv, _ := newServer(t) // the mock fails on any call reaching it

	for name, c := range map[string]call{
		"no token":      {method: "GET", path: "/admin/v1/tenants", noAuth: true},
		"wrong token":   {method: "GET", path: "/admin/v1/tenants", noAuth: true, header: map[string]string{"Authorization": "Bearer nope"}},
		"token prefix":  {method: "GET", path: "/admin/v1/tenants", noAuth: true, header: map[string]string{"Authorization": "Bearer " + testToken[:10]}},
		"unknown route": {method: "GET", path: "/admin/v1/secrets", noAuth: true},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := do(t, srv, c)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") != "Bearer" || errorType(t, body) != "authentication_error" {
				t.Errorf("headers %v body %s", resp.Header, body)
			}
		})
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := do(t, srv, call{method: "GET", path: "/healthz", noAuth: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestActorIsTheCredentialName(t *testing.T) {
	srv, admin := newServer(t)

	admin.EXPECT().DisableTenant(gomock.Any(), gomock.Any(), "acme").DoAndReturn(
		func(_ context.Context, w ports.Write, _ string) (domains.TenantAccount, error) {
			if w.Actor != "admin:billing-sync" {
				t.Errorf("actor = %q", w.Actor)
			}
			return acme(), nil
		})

	resp, _ := do(t, srv, call{method: "DELETE", path: "/admin/v1/tenants/acme"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// --- tenants -----------------------------------------------------------

func TestCreateTenantStatusSaysWhetherItWasNew(t *testing.T) {
	srv, admin := newServer(t)

	admin.EXPECT().CreateTenant(gomock.Any(), gomock.Any(), ports.NewTenant{
		ExternalID: "acme", Name: "Acme", Metadata: []byte(`{"plan":"pro"}`),
	}).Return(ports.TenantCreated{Tenant: acme(), Created: true}, nil)
	admin.EXPECT().CreateTenant(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.TenantCreated{Tenant: acme()}, nil)

	body := `{"external_id":"acme","name":"Acme","metadata":{"plan":"pro"}}`
	resp, got := do(t, srv, call{method: "POST", path: "/admin/v1/tenants", body: body})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: status = %d, body %s", resp.StatusCode, got)
	}
	var tenant gen.Tenant
	if err := json.Unmarshal([]byte(got), &tenant); err != nil || tenant.Id != tenantID || tenant.ExternalId != "acme" {
		t.Fatalf("body = %s", got)
	}

	resp, _ = do(t, srv, call{method: "POST", path: "/admin/v1/tenants", body: body})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat create: status = %d, want 200", resp.StatusCode)
	}
}

func TestMalformedBodiesAreRejectedBeforeTheUseCase(t *testing.T) {
	srv, _ := newServer(t)

	for name, c := range map[string]call{
		"not json":       {method: "POST", path: "/admin/v1/tenants", body: `{`},
		"unknown field":  {method: "POST", path: "/admin/v1/tenants", body: `{"external_id":"a","name":"A","plan":"pro"}`},
		"trailing data":  {method: "POST", path: "/admin/v1/tenants", body: `{"external_id":"a","name":"A"} {}`},
		"empty create":   {method: "POST", path: "/admin/v1/tenants"},
		"bad key id":     {method: "GET", path: "/admin/v1/keys/not-a-uuid"},
		"patch key bare": {method: "PATCH", path: "/admin/v1/keys/" + keyUUID.String(), body: `{}`},
		"allowlist type": {method: "PATCH", path: "/admin/v1/keys/" + keyUUID.String(), body: `{"model_allowlist":"chat"}`},
		"quotas missing": {method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{}`},
		"quota typo":     {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"max_cost":5}`},
		"quota window":   {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"window_kind":"hour","max_cost_nanos":5}`},
		"quota capless":  {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"on_exceed":"block"}`},
		"quota retired":  {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"max_cost_nanos":5,"max_requests":60}`},
		"set retired":    {method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{"quotas":[{"window_kind":"day","max_cost_nanos":5,"max_tokens":60}]}`},
		"bad limit":      {method: "GET", path: "/admin/v1/tenants?limit=ten"},
		"limit too high": {method: "GET", path: "/admin/v1/tenants?limit=1001"},
		"bad enum":       {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"max_cost_nanos":5,"on_exceed":"warn"}`},
		"bad window":     {method: "GET", path: "/admin/v1/tenants/acme/quotas/fortnight"},
		"set item typo":  {method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{"quotas":[{"window_kind":"day","max_cost":5}]}`},
		"negative limit": {method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"max_cost_nanos":-1}`},
		"bad date":       {method: "PUT", path: "/admin/v1/tenants/acme/quotas/period", body: `{"period_start":"yesterday","max_cost_nanos":1}`},
		"metadata array": {method: "POST", path: "/admin/v1/tenants", body: `{"external_id":"a","name":"A","metadata":[1]}`},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := do(t, srv, c)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", resp.StatusCode, body)
			}
			if errorType(t, body) != "invalid_request_error" {
				t.Errorf("body = %s", body)
			}
		})
	}
}

func TestListTenantsPassesTheFilter(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().ListTenants(gomock.Any(), ports.TenantFilter{IncludeDisabled: true, Limit: 5}).
		Return([]domains.TenantAccount{acme()}, nil)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants?include_disabled=true&limit=5"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"tenants":[{`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestErrorsMapToStatuses(t *testing.T) {
	tests := []struct {
		err    error
		status int
		typ    string
	}{
		{fmt.Errorf("%w: tenant %q", ports.ErrNotFound, "x"), 404, "not_found_error"},
		{fmt.Errorf("%w: taken", ports.ErrConflict), 409, "conflict_error"},
		{fmt.Errorf("%w: bad", ports.ErrInvalid), 400, "invalid_request_error"},
		{fmt.Errorf("postgres: get tenant: %w", context.DeadlineExceeded), 504, "timeout_error"},
		{errors.New("postgres: connection reset"), 500, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			srv, admin := newServer(t)
			admin.EXPECT().GetTenant(gomock.Any(), "x").Return(domains.TenantAccount{}, tt.err)

			resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/x"})
			if resp.StatusCode != tt.status || errorType(t, body) != tt.typ {
				t.Fatalf("status %d body %s, want %d %s", resp.StatusCode, body, tt.status, tt.typ)
			}
			if tt.status == 500 && strings.Contains(body, "postgres") {
				t.Error("a database error must not leak to the caller")
			}
		})
	}
}

// --- keys --------------------------------------------------------------

func TestCreateAPIKeyShowsTheSecretOnce(t *testing.T) {
	srv, admin := newServer(t)
	const secret = "gr_live_supersecretvalue"

	admin.EXPECT().CreateAPIKey(gomock.Any(), gomock.Any(), "acme", ports.NewAPIKey{ModelAllowlist: []string{"chat"}}).
		Return(ports.IssuedAPIKey{
			Key:    domains.APIKey{ID: keyUUID, TenantID: tenantID, Prefix: "gr_live_supers", CreatedAt: created},
			Secret: secret,
		}, nil)

	resp, body := do(t, srv, call{method: "POST", path: "/admin/v1/tenants/acme/keys",
		body: `{"model_allowlist":["chat"]}`})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var live gen.IssuedAPIKey
	if err := json.Unmarshal([]byte(body), &live); err != nil {
		t.Fatal(err)
	}
	if live.Secret != secret || live.Id != keyUUID {
		t.Errorf("live response = %s", body)
	}
}

func TestRepeatedCreateAPIKeyMintsASecondKey(t *testing.T) {
	srv, admin := newServer(t)
	second := uuid.MustParse("0192a000-0000-7000-8000-0000000000bb")

	gomock.InOrder(
		admin.EXPECT().CreateAPIKey(gomock.Any(), gomock.Any(), "acme", gomock.Any()).
			Return(ports.IssuedAPIKey{Key: domains.APIKey{ID: keyUUID}, Secret: "gr_live_one"}, nil),
		admin.EXPECT().CreateAPIKey(gomock.Any(), gomock.Any(), "acme", gomock.Any()).
			Return(ports.IssuedAPIKey{Key: domains.APIKey{ID: second}, Secret: "gr_live_two"}, nil),
	)

	c := call{method: "POST", path: "/admin/v1/tenants/acme/keys"}
	first, firstBody := do(t, srv, c)
	repeat, repeatBody := do(t, srv, c)
	if first.StatusCode != http.StatusCreated || repeat.StatusCode != http.StatusCreated {
		t.Fatalf("statuses %d and %d", first.StatusCode, repeat.StatusCode)
	}
	if !strings.Contains(firstBody, keyUUID.String()) || !strings.Contains(repeatBody, second.String()) {
		t.Errorf("first = %s, repeat = %s", firstBody, repeatBody)
	}
}

func TestCreateAPIKeyWithNoBodyAllowsEveryAlias(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().CreateAPIKey(gomock.Any(), gomock.Any(), "acme", ports.NewAPIKey{}).
		Return(ports.IssuedAPIKey{Key: domains.APIKey{ID: keyUUID}, Secret: "gr_live_x"}, nil)

	resp, body := do(t, srv, call{method: "POST", path: "/admin/v1/tenants/acme/keys"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"model_allowlist":null`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestPatchAPIKeyTellsNullFromEmpty(t *testing.T) {
	srv, admin := newServer(t)

	gomock.InOrder(
		admin.EXPECT().UpdateAPIKey(gomock.Any(), gomock.Any(), keyUUID, gomock.Any()).DoAndReturn(
			func(_ context.Context, _ ports.Write, _ uuid.UUID, p ports.APIKeyPatch) (domains.APIKey, error) {
				if p.ModelAllowlist != nil {
					t.Errorf("null became %#v, want nil (every alias)", p.ModelAllowlist)
				}
				return domains.APIKey{ID: keyUUID}, nil
			}),
		admin.EXPECT().UpdateAPIKey(gomock.Any(), gomock.Any(), keyUUID, gomock.Any()).DoAndReturn(
			func(_ context.Context, _ ports.Write, _ uuid.UUID, p ports.APIKeyPatch) (domains.APIKey, error) {
				if p.ModelAllowlist == nil || len(p.ModelAllowlist) != 0 {
					t.Errorf("[] became %#v, want empty (no alias)", p.ModelAllowlist)
				}
				return domains.APIKey{ID: keyUUID, ModelAllowlist: []string{}}, nil
			}),
	)

	path := "/admin/v1/keys/" + keyUUID.String()
	do(t, srv, call{method: "PATCH", path: path, body: `{"model_allowlist":null}`})
	_, body := do(t, srv, call{method: "PATCH", path: path, body: `{"model_allowlist":[]}`})
	if !strings.Contains(body, `"model_allowlist":[]`) {
		t.Errorf("body = %s", body)
	}
}

// --- quotas ------------------------------------------------------------

func TestReplaceQuotasDecodesTheSet(t *testing.T) {
	srv, admin := newServer(t)

	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	admin.EXPECT().ReplaceQuotas(gomock.Any(), gomock.Any(), "acme", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ ports.Write, _ string, set []domains.Quota) ([]domains.Quota, error) {
			if len(set) != 2 {
				t.Fatalf("set = %+v", set)
			}
			p := set[1]
			if p.WindowKind != domains.WindowPeriod || !p.PeriodStart.Equal(start) ||
				p.MaxCost != 5_000_000_000 || p.OnExceed != domains.QuotaAllow {
				t.Errorf("period quota = %+v", p)
			}
			return set, nil
		})

	resp, body := do(t, srv, call{method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{"quotas":[
		{"window_kind":"minute","max_cost_nanos":60},
		{"window_kind":"period","period_start":"2026-09-14T00:00:00Z","period_end":"2026-10-14T00:00:00Z",
		 "max_cost_nanos":5000000000,"on_exceed":"allow"}
	]}`})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"max_cost_nanos":5000000000`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestReplaceQuotasAcceptsAnExplicitEmptySet(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().ReplaceQuotas(gomock.Any(), gomock.Any(), "acme", []domains.Quota{}).Return(nil, nil)

	resp, body := do(t, srv, call{method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{"quotas":[]}`})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"quotas":[]`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestPutQuotaTakesTheWindowFromThePath(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().PutQuota(gomock.Any(), gomock.Any(), "acme", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ ports.Write, _ string, q domains.Quota) (domains.Quota, error) {
			if q.WindowKind != domains.WindowDay {
				t.Errorf("window = %q", q.WindowKind)
			}
			return q, nil
		})

	resp, _ := do(t, srv, call{method: "PUT", path: "/admin/v1/tenants/acme/quotas/day", body: `{"max_cost_nanos":100}`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestDeleteQuotaIsNoContent(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().DeleteQuota(gomock.Any(), gomock.Any(), "acme", domains.WindowMonth).Return(nil)

	resp, body := do(t, srv, call{method: "DELETE", path: "/admin/v1/tenants/acme/quotas/month"})
	if resp.StatusCode != http.StatusNoContent || body != "" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

// --- contract enforcement ------------------------------------------------

func TestValidationErrorsNameTheField(t *testing.T) {
	srv, _ := newServer(t)

	for name, tt := range map[string]struct {
		c    call
		want []string
	}{
		"unknown field": {
			call{method: "POST", path: "/admin/v1/tenants", body: `{"external_id":"a","name":"A","plan":"pro"}`},
			[]string{"request body", `"plan"`, "unsupported"},
		},
		"bad enum": {
			call{method: "PUT", path: "/admin/v1/tenants/acme/quotas", body: `{"quotas":[{"window_kind":"day","max_cost_nanos":1,"on_exceed":"warn"}]}`},
			[]string{"quotas.0.on_exceed", "allowed values"},
		},
		"missing field": {
			call{method: "PATCH", path: "/admin/v1/keys/" + keyUUID.String(), body: `{}`},
			[]string{"model_allowlist"},
		},
		"bad query": {
			call{method: "GET", path: "/admin/v1/tenants?limit=ten"},
			[]string{"query parameter limit"},
		},
		"bad uuid": {
			call{method: "GET", path: "/admin/v1/keys/not-a-uuid"},
			[]string{"parameter key", "UUID"},
		},
		"bad date": {
			call{method: "PUT", path: "/admin/v1/tenants/acme/quotas/period", body: `{"period_start":"yesterday","max_cost_nanos":1}`},
			[]string{"period_start", "date-time"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := do(t, srv, tt.c)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", resp.StatusCode, body)
			}
			msg := errorBody(t, body).Message
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q does not mention %q", msg, w)
				}
			}
			if strings.Contains(msg, "\n") || len(msg) > 200 {
				t.Errorf("message is a wall of text: %q", msg)
			}
		})
	}
}

func TestUnknownRoutesAndMethodsUseTheEnvelope(t *testing.T) {
	srv, _ := newServer(t)

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/secrets"})
	if resp.StatusCode != http.StatusNotFound || errorType(t, body) != "not_found_error" {
		t.Errorf("unknown route: status %d body %s", resp.StatusCode, body)
	}
	resp, body = do(t, srv, call{method: "PUT", path: "/admin/v1/tenants"})
	if resp.StatusCode != http.StatusMethodNotAllowed || errorType(t, body) != "invalid_request_error" {
		t.Errorf("wrong method: status %d body %s", resp.StatusCode, body)
	}
}

func TestBodyIsJSONWhateverTheContentType(t *testing.T) {
	srv, admin := newServer(t)
	admin.EXPECT().CreateTenant(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.TenantCreated{Tenant: acme(), Created: true}, nil)

	resp, body := do(t, srv, call{method: "POST", path: "/admin/v1/tenants",
		body:   `{"external_id":"acme","name":"Acme"}`,
		header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestBodyOverTheLimitIs413(t *testing.T) {
	srv, _ := newServer(t)
	big := `{"external_id":"a","name":"` + strings.Repeat("x", maxBodyBytes) + `"}`

	resp, body := do(t, srv, call{method: "POST", path: "/admin/v1/tenants", body: big})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || errorType(t, body) != "invalid_request_error" {
		t.Fatalf("status %d body %.200s", resp.StatusCode, body)
	}
}

// --- context -------------------------------------------------------------

func TestUseCaseGetsTheRequestContext(t *testing.T) {
	srv, admin := newServer(t)

	admin.EXPECT().UpdateTenant(gomock.Any(), gomock.Any(), "acme", gomock.Any()).DoAndReturn(
		func(ctx context.Context, w ports.Write, _ string, p ports.TenantPatch) (domains.TenantAccount, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				t.Errorf("deadline = %v, %v; want one within the request timeout", deadline, ok)
			}
			if w.Actor != "admin:billing-sync" {
				t.Errorf("write = %+v", w)
			}
			if p.Name == nil || *p.Name != "A" || string(p.Metadata) != `{"n":12345678901234567890}` {
				t.Errorf("patch = %+v (metadata %s)", p, p.Metadata)
			}
			return acme(), nil
		})

	resp, body := do(t, srv, call{method: "PATCH", path: "/admin/v1/tenants/acme",
		body: `{"name":"A","metadata":{"n":12345678901234567890}}`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestSlowUseCaseTimesOutWith504(t *testing.T) {
	srv, admin := newServerWithTimeout(t, 50*time.Millisecond)

	admin.EXPECT().ListTenants(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ ports.TenantFilter) ([]domains.TenantAccount, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("postgres: list tenants: %w", ctx.Err())
		})

	start := time.Now()
	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants"})
	if resp.StatusCode != http.StatusGatewayTimeout || errorType(t, body) != "timeout_error" {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "postgres") {
		t.Error("a database error must not leak to the caller")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; the deadline did not bound the request", elapsed)
	}
}

func TestTimeoutIsRecognisedFromTheContext(t *testing.T) {
	srv, admin := newServerWithTimeout(t, 50*time.Millisecond)
	admin.EXPECT().GetTenant(gomock.Any(), "acme").DoAndReturn(
		func(ctx context.Context, _ string) (domains.TenantAccount, error) {
			<-ctx.Done()
			return domains.TenantAccount{}, errors.New("conn closed")
		})

	resp, body := do(t, srv, call{method: "GET", path: "/admin/v1/tenants/acme"})
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}
