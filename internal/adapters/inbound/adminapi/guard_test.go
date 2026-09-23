package adminapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// The allowlist is pinned here rather than derived, so widening it is a
// deliberate edit to this list and not a side effect of adding a path.
func TestOnlyLoginAndRefreshAreOpen(t *testing.T) {
	want := []string{
		"POST /admin/v1/auth/login",
		"POST /admin/v1/auth/refresh",
	}
	if got := UnauthenticatedRoutes(); !slices.Equal(got, want) {
		t.Errorf("unauthenticated routes = %v, want %v", got, want)
	}
}

// Every operation the spec declares, walked from the spec itself, so a path
// added later is covered by this test without being listed in it.
func TestEverySpecRouteOutsideTheAllowlistNeedsAToken(t *testing.T) {
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}

	open := UnauthenticatedRoutes()
	checked := 0

	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/admin/") {
			continue
		}
		for method := range item.Operations() {
			// A templated path is requested with a value in place of the
			// parameter; the guard matches on the concrete path either way.
			concrete := strings.NewReplacer("{tenant}", "acme", "{key}", "k", "{window}", "day",
				"{decision_id}", "d").Replace(path)

			t.Run(method+" "+path, func(t *testing.T) {
				req := httptest.NewRequest(method, concrete, strings.NewReader(`{}`))
				rec := serveWithUsers(t, newUsers(testUser), &sessions{}, req)

				if slices.Contains(open, method+" "+concrete) {
					if rec.Code == http.StatusUnauthorized {
						t.Errorf("an unauthenticated path answered 401: %s", rec.Body)
					}
					return
				}
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("code = %d, body = %s; this route is reachable without a token",
						rec.Code, rec.Body)
				}
			})
			checked++
		}
	}

	if checked < 10 {
		t.Fatalf("only %d operations were walked; the spec was not read", checked)
	}
}

// A path that only looks like an allowlisted one falls to the guarded branch,
// because the match is exact and everything else fails closed.
func TestNearMissesOfTheAllowlistAreGuarded(t *testing.T) {
	for name, route := range map[string]struct{ method, path string }{
		"trailing slash":  {http.MethodPost, "/admin/v1/auth/login/"},
		"different case":  {http.MethodPost, "/admin/v1/auth/LOGIN"},
		"wrong method":    {http.MethodGet, "/admin/v1/auth/login"},
		"prefix only":     {http.MethodPost, "/admin/v1/auth/loginx"},
		"nested under it": {http.MethodPost, "/admin/v1/auth/login/tenants"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			rec := serveWithUsers(t, newUsers(testUser), &sessions{}, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}
}
