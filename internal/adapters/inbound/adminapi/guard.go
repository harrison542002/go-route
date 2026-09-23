package adminapi

import (
	"net/http"
	"sort"
)

var openPaths = map[string]struct{}{
	"POST /admin/v1/auth/login":   {},
	"POST /admin/v1/auth/refresh": {},
}

var selfServicePaths = map[string]struct{}{
	"POST /admin/v1/auth/logout": {},
}

// Guard applies authentication and the role check to everything under /admin/
// except the paths named above.
func Guard(authenticate, permit func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		var (
			open        = next
			selfService = authenticate(next)
			guarded     = authenticate(permit(next))
		)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := r.Method + " " + r.URL.Path
			switch {
			case has(openPaths, route):
				open.ServeHTTP(w, r)
			case has(selfServicePaths, route):
				selfService.ServeHTTP(w, r)
			default:
				guarded.ServeHTTP(w, r)
			}
		})
	}
}

func has(set map[string]struct{}, route string) bool {
	_, ok := set[route]
	return ok
}

// UnauthenticatedRoutes lists every route reachable without a token, sorted,
// for a test to assert against and for nothing else.
func UnauthenticatedRoutes() []string {
	routes := make([]string, 0, len(openPaths))
	for route := range openPaths {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	return routes
}
