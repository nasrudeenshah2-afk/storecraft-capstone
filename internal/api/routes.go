package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Route is one documented (method, path) pair, e.g. {"GET", "/v1/keys/{key}"}.
type Route struct {
	Method string
	Path   string
}

// routeSpec is the single source of truth for what the router serves. The mux
// is built from it, 405 Allow headers come from it, metrics labels use its
// path, and api/openapi.yaml must document exactly these routes (see the
// coverage test). Add or change routes here, and in the spec, together.
type routeSpec struct {
	path    string   // documented path; also the metrics "route" label
	pattern string   // method-less ServeMux pattern
	methods []string // in the order shown in the Allow header
}

var routeSpecs = []routeSpec{
	{"/v1/keys/{key}", "/v1/keys/{key...}", []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{"/v1/keys", "/v1/keys", []string{http.MethodGet}},
	{"/v1/changes", "/v1/changes", []string{http.MethodGet}},
	{"/v1/changes/stream", "/v1/changes/stream", []string{http.MethodGet}},
	{"/metrics", "/metrics", []string{http.MethodGet}},
	{"/v1/admin/changes", "/v1/admin/changes", []string{http.MethodGet}},
	{"/v1/replication/status", "/v1/replication/status", []string{http.MethodGet}},
	{"/v1/admin/digest", "/v1/admin/digest", []string{http.MethodGet}},

	// STORE-210.
	{"/v1/usage", "/v1/usage", []string{http.MethodGet}},
	{"/v1/admin/tenants", "/v1/admin/tenants", []string{http.MethodGet}},
	{"/v1/admin/tenants/{id}", "/v1/admin/tenants/{id}", []string{http.MethodGet}},
	{"/v1/admin/tenants/{id}/quota", "/v1/admin/tenants/{id}/quota", []string{http.MethodGet, http.MethodPut}},
	{"/v1/flags", "/v1/flags", []string{http.MethodGet}},
	{"/v1/admin/flags/quota_enforcement", "/v1/admin/flags/quota_enforcement", []string{http.MethodPut}},
}

// unmatchedRoute is the metrics label for requests that match no route, so
// arbitrary paths can never become label values.
const unmatchedRoute = "unmatched"

// Routes lists every (method, path) the router serves, sorted by path then
// method. Tests use it to check the OpenAPI document.
func Routes() []Route {
	var out []Route
	for _, rs := range routeSpecs {
		for _, m := range rs.methods {
			out = append(out, Route{Method: m, Path: rs.path})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// handlers maps "METHOD /documented/path" to its handler.
func (s *server) handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/keys/{key}":     s.withKey(s.get),
		"PUT /v1/keys/{key}":     s.withKey(s.put),
		"DELETE /v1/keys/{key}":  s.withKey(s.delete),
		"GET /v1/keys":           s.listRoute,
		"GET /v1/changes":        s.changesRoute,
		"GET /v1/changes/stream": s.streamRoute,
		"GET /metrics":           s.metricsRoute,

		"GET /v1/admin/changes":      s.adminChangesRoute,
		"GET /v1/replication/status": s.replicationStatusRoute,
		"GET /v1/admin/digest":       s.digestRoute,

		"GET /v1/usage":                         s.usageRoute,
		"GET /v1/admin/tenants":                 s.tenantsRoute,
		"GET /v1/admin/tenants/{id}":            s.tenantRoute,
		"GET /v1/admin/tenants/{id}/quota":      s.quotaRoute,
		"PUT /v1/admin/tenants/{id}/quota":      s.putQuotaRoute,
		"GET /v1/flags":                         s.flagsRoute,
		"PUT /v1/admin/flags/quota_enforcement": s.putFlagRoute,
	}
}

// mux builds the router from routeSpecs. It panics (a programming error, at
// startup) if a spec'd method has no handler or a handler has no spec.
func (s *server) mux() *http.ServeMux {
	handlers := s.handlers()
	used := map[string]bool{}
	mux := http.NewServeMux()
	for _, rs := range routeSpecs {
		byMethod := map[string]http.HandlerFunc{}
		for _, m := range rs.methods {
			key := m + " " + rs.path
			h, ok := handlers[key]
			if !ok {
				panic(fmt.Sprintf("api: route %q has no handler", key))
			}
			byMethod[m] = h
			used[key] = true
		}
		allow := strings.Join(rs.methods, ", ")
		// Method-less pattern so wrong methods reach us and get a JSON 405.
		mux.HandleFunc(rs.pattern, s.instrument(rs.path, func(w http.ResponseWriter, r *http.Request) {
			h, ok := byMethod[r.Method]
			if !ok {
				methodNotAllowed(w, allow)
				return
			}
			h(w, r)
		}))
	}
	for key := range handlers {
		if !used[key] {
			panic(fmt.Sprintf("api: handler %q has no route spec", key))
		}
	}
	mux.HandleFunc("/", s.instrument(unmatchedRoute, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such route")
	}))
	return mux
}
