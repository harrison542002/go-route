package adminapi

import (
	"net/http"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

func RequirePermission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !roleFrom(r.Context()).Allows(permissionFor(r.Method)) {
			writeError(w, http.StatusForbidden,
				"this credential is read-only and may not make changes", gen.PermissionError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func permissionFor(method string) domains.Permission {
	switch method {
	case http.MethodGet, http.MethodHead:
		return domains.PermissionRead
	default:
		return domains.PermissionWrite
	}
}
