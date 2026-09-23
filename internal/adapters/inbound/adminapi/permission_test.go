package adminapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// mutating is every method the spec puts a change behind, plus ones it does not
// use: anything that is not a known safe method has to fail closed.
var mutating = []string{
	http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete,
	http.MethodOptions, "PROPFIND",
}

var safe = []string{http.MethodGet, http.MethodHead}

// servePermission runs one request through RequireToken and RequirePermission
// as the server chains them.
func servePermission(t *testing.T, cred domains.AdminCredential, method string) (int, string) {
	t.Helper()

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(method, "/admin/v1/tenants/acme", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	RequireToken(newCredentials(testToken, cred), newUsers(testUser), testIssuer(t))(
		RequirePermission(next)).ServeHTTP(rec, req)

	if rec.Code == http.StatusNoContent != reached {
		t.Fatalf("handler reached = %v with status %d", reached, rec.Code)
	}
	return rec.Code, rec.Body.String()
}

func readonlyCredential() domains.AdminCredential {
	cred := testCredential
	cred.Role = domains.RoleReadonly
	return cred
}

func TestReadonlyCredentialMayRead(t *testing.T) {
	for _, method := range safe {
		t.Run(method, func(t *testing.T) {
			if code, body := servePermission(t, readonlyCredential(), method); code != http.StatusNoContent {
				t.Errorf("code = %d, body = %s", code, body)
			}
		})
	}
}

func TestReadonlyCredentialMayNotChangeAnything(t *testing.T) {
	for _, method := range mutating {
		t.Run(method, func(t *testing.T) {
			code, body := servePermission(t, readonlyCredential(), method)
			if code != http.StatusForbidden {
				t.Fatalf("code = %d, body = %s", code, body)
			}
			if !strings.Contains(body, "permission_error") || !strings.Contains(body, "read-only") {
				t.Errorf("body = %s", body)
			}
		})
	}
}

func TestAdminCredentialMayDoEverything(t *testing.T) {
	for _, method := range append(append([]string{}, safe...), mutating...) {
		t.Run(method, func(t *testing.T) {
			if code, body := servePermission(t, testCredential, method); code != http.StatusNoContent {
				t.Errorf("code = %d, body = %s", code, body)
			}
		})
	}
}

func TestPermissionWithoutAuthenticationRefuses(t *testing.T) {
	for _, method := range append(append([]string{}, safe...), mutating...) {
		req := httptest.NewRequest(method, "/admin/v1/tenants/acme", nil)
		rec := httptest.NewRecorder()
		RequirePermission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Errorf("%s: an unauthenticated request reached the handler", method)
		})).ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code = %d", method, rec.Code)
		}
	}
}

func TestPermissionForMethod(t *testing.T) {
	for _, method := range safe {
		if got := permissionFor(method); got != domains.PermissionRead {
			t.Errorf("permissionFor(%q) = %q", method, got)
		}
	}
	for _, method := range mutating {
		if got := permissionFor(method); got != domains.PermissionWrite {
			t.Errorf("permissionFor(%q) = %q", method, got)
		}
	}
}
