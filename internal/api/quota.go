package api

// STORE-210 routes: usage, tenant list, quotas and the quota_enforcement flag
// (docs/capstone/spec.md section 4, api/openapi.yaml).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

type usageCounterJSON struct {
	Used  int64  `json:"used"`
	Limit *int64 `json:"limit"` // null = unlimited
}

type usageJSON struct {
	TenantID int64            `json:"tenant_id"`
	Keys     usageCounterJSON `json:"keys"`
	Bytes    usageCounterJSON `json:"bytes"`
	Seq      int64            `json:"seq"`
}

type quotaJSON struct {
	MaxKeys  *int64 `json:"max_keys"`
	MaxBytes *int64 `json:"max_bytes"`
	Version  int64  `json:"version"`
}

type tenantUsageJSON struct {
	Keys  int64 `json:"keys"`
	Bytes int64 `json:"bytes"`
}

type tenantSummaryJSON struct {
	ID    int64           `json:"id"`
	Name  string          `json:"name"`
	Quota quotaJSON       `json:"quota"`
	Usage tenantUsageJSON `json:"usage"`
}

type flagJSON struct {
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
}

type flagsJSON struct {
	QuotaEnforcement flagJSON `json:"quota_enforcement"`
}

func toQuotaJSON(q store.Quota) quotaJSON {
	return quotaJSON{MaxKeys: q.MaxKeys, MaxBytes: q.MaxBytes, Version: q.Version}
}

func toTenantSummaryJSON(t store.TenantSummary) tenantSummaryJSON {
	return tenantSummaryJSON{ID: t.ID, Name: t.Name, Quota: toQuotaJSON(t.Quota),
		Usage: tenantUsageJSON{Keys: t.Usage.Keys, Bytes: t.Usage.Bytes}}
}

// usageRoute serves GET /v1/usage: the signed-in tenant's own usage and
// limits. There is no parameter naming a tenant, so a tenant can never read
// another tenant's usage.
func (s *server) usageRoute(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	u, err := s.st.UsageAt(r.Context(), t.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, usageJSON{
		TenantID: t.ID,
		Keys:     usageCounterJSON{Used: u.Usage.Keys, Limit: u.Quota.MaxKeys},
		Bytes:    usageCounterJSON{Used: u.Usage.Bytes, Limit: u.Quota.MaxBytes},
		Seq:      u.Seq,
	})
}

// tenantsRoute serves GET /v1/admin/tenants (admin): every tenant with quota
// and usage, sorted by id.
func (s *server) tenantsRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ts, err := s.st.Tenants(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]tenantSummaryJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTenantSummaryJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": out})
}

// tenantID reads the {id} path value. On a bad value it writes a 400.
func tenantID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "tenant id must be a positive integer")
		return 0, false
	}
	return id, true
}

// tenantRoute serves GET /v1/admin/tenants/{id} (admin).
func (s *server) tenantRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	t, err := s.st.TenantSummary(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTenantSummaryJSON(t))
}

// quotaRoute serves GET /v1/admin/tenants/{id}/quota (admin); the ETag is
// the quota version (0 = the default quota, never edited).
func (s *server) quotaRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	q, err := s.st.Quota(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(q.Version))
	writeJSON(w, http.StatusOK, toQuotaJSON(q))
}

// requiredVersion reads the If-Match header of a CAS edit: 428 when it is
// missing, 400 when it is not a version number (0 is allowed: never edited).
func requiredVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		writeError(w, http.StatusPreconditionRequired, codePreconditionRequired,
			"If-Match with the current version is required; read it first (GET), then send it back")
		return 0, false
	}
	v, err := strconv.ParseInt(strings.Trim(h, `"`), 10, 64)
	if err != nil || v < 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "If-Match must be a version number")
		return 0, false
	}
	return v, true
}

// decodeStrictObject reads the body as exactly one JSON object whose field
// names are all in allowed and returns its fields. On failure it writes a 400.
func decodeStrictObject(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]json.RawMessage, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	bad := func(msg string) (map[string]json.RawMessage, bool) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, msg)
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return bad("body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("body must be a single JSON object")
	}
	for name := range fields {
		known := false
		for _, a := range allowed {
			known = known || name == a
		}
		if !known {
			return bad("unknown field " + strconv.Quote(name))
		}
	}
	return fields, true
}

// parseLimit reads a quota limit: null (unlimited) or an integer >= 0.
func parseLimit(raw json.RawMessage) (*int64, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	n, err := strconv.ParseInt(string(raw), 10, 64) // rejects 1.5, 1e3, "10"
	if err != nil || n < 0 {
		return nil, false
	}
	return &n, true
}

// putQuotaRoute serves PUT /v1/admin/tenants/{id}/quota (admin, CAS).
func (s *server) putQuotaRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	if s.rejectIfReplica(w) {
		return
	}
	expected, ok := requiredVersion(w, r)
	if !ok {
		return
	}
	fields, ok := decodeStrictObject(w, r, "max_keys", "max_bytes")
	if !ok {
		return
	}
	var limits [2]*int64
	for i, name := range []string{"max_keys", "max_bytes"} {
		raw, present := fields[name]
		if !present {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, `both "max_keys" and "max_bytes" are required (null = unlimited)`)
			return
		}
		v, ok := parseLimit(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, strconv.Quote(name)+" must be a whole number of 0 or more, or null")
			return
		}
		limits[i] = v
	}
	prev, next, err := s.st.SetQuota(r.Context(), id, limits[0], limits[1], expected)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, "the quota changed since you read it; read it again")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	s.log.Info("quota changed", "tenant_id", id,
		"old_max_keys", limitText(prev.MaxKeys), "old_max_bytes", limitText(prev.MaxBytes), "old_version", prev.Version,
		"new_max_keys", limitText(next.MaxKeys), "new_max_bytes", limitText(next.MaxBytes), "new_version", next.Version)
	w.Header().Set("ETag", etag(next.Version))
	writeJSON(w, http.StatusOK, toQuotaJSON(next))
}

func limitText(n *int64) string {
	if n == nil {
		return "unlimited"
	}
	return strconv.FormatInt(*n, 10)
}

// flagsRoute serves GET /v1/flags for a tenant or the admin token.
func (s *server) flagsRoute(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}
	if hash := auth.HashToken(token); !s.isAdminHash(hash) {
		if _, err := s.st.TenantByTokenHash(r.Context(), hash); errors.Is(err, store.ErrNotFound) {
			writeUnauthorized(w)
			return
		} else if err != nil {
			s.internal(w, r, err)
			return
		}
	}
	f, err := s.st.Flag(r.Context(), store.FlagQuotaEnforcement)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, flagsJSON{QuotaEnforcement: flagJSON{Enabled: f.Enabled, Version: f.Version}})
}

// putFlagRoute serves PUT /v1/admin/flags/quota_enforcement (admin, CAS). The
// very next write reads the new state: the flag is read from the database on
// every use (store.Flag / the quota check), never cached.
func (s *server) putFlagRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.rejectIfReplica(w) {
		return
	}
	expected, ok := requiredVersion(w, r)
	if !ok {
		return
	}
	fields, ok := decodeStrictObject(w, r, "enabled")
	if !ok {
		return
	}
	raw, present := fields["enabled"]
	if !present || (string(raw) != "true" && string(raw) != "false") {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, `"enabled" must be true or false`)
		return
	}
	enabled := string(raw) == "true"
	prev, next, err := s.st.SetFlag(r.Context(), store.FlagQuotaEnforcement, enabled, expected)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, "the flag changed since you read it; read it again")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Info("flag changed", "flag", store.FlagQuotaEnforcement,
		"old_enabled", prev.Enabled, "old_version", prev.Version, "new_enabled", next.Enabled, "new_version", next.Version)
	w.Header().Set("ETag", etag(next.Version))
	writeJSON(w, http.StatusOK, flagJSON{Enabled: next.Enabled, Version: next.Version})
}
