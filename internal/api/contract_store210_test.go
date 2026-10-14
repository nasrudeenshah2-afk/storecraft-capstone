package api

// Contract tests for STORE-210 (docs/capstone/spec.md section 4,
// api/openapi.yaml). They are written BEFORE the endpoints exist (step B3)
// and are expected to FAIL until the server implements them (step B7).
// They check the contract only: status codes, the error envelope, JSON field
// shapes and the ETag / If-Match rules. Behaviour (limits, concurrency,
// replication) is covered by the acceptance tests of step B5.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// contractNotImplemented fails with one clear line while the route is missing.
func contractNotImplemented(t *testing.T, r resp, what string) {
	t.Helper()
	if r.status == 404 && r.body != "" && json.Valid([]byte(r.body)) {
		var env struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal([]byte(r.body), &env)
		if env.Error.Message == "no such route" {
			t.Fatalf("STORE-210 contract: %s is not implemented yet (404 no such route)", what)
		}
	}
}

func decodeJSON(t *testing.T, r resp, into any) {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (body %q)", ct, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), into); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

// fieldsOf returns the JSON object's fields, to check exact shapes.
func fieldsOf(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %s", raw)
	}
	return m
}

func requireFields(t *testing.T, what string, m map[string]json.RawMessage, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Errorf("%s: missing field %q (have %v)", what, n, keysOfMap(m))
		}
	}
}

func keysOfMap(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestContractSTORE210_GetUsage(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	r := e.do("GET", "/v1/usage", e.tokenA, "", nil)
	contractNotImplemented(t, r, "GET /v1/usage")
	e.expect(r, 200, "")

	var raw json.RawMessage
	decodeJSON(t, r, &raw)
	top := fieldsOf(t, raw)
	requireFields(t, "Usage", top, "tenant_id", "keys", "bytes", "seq")
	for _, dim := range []string{"keys", "bytes"} {
		requireFields(t, "Usage."+dim, fieldsOf(t, top[dim]), "used", "limit")
	}
	var u struct {
		TenantID int64 `json:"tenant_id"`
		Keys     struct {
			Used  int64  `json:"used"`
			Limit *int64 `json:"limit"`
		} `json:"keys"`
		Bytes struct {
			Used  int64  `json:"used"`
			Limit *int64 `json:"limit"`
		} `json:"bytes"`
	}
	decodeJSON(t, r, &u)
	if u.TenantID != 1 || u.Keys.Limit == nil || *u.Keys.Limit != 1000 || u.Bytes.Limit == nil || *u.Bytes.Limit != 1048576 {
		t.Fatalf("default usage = %s, want tenant 1 with limits 1000 keys / 1048576 bytes", r.body)
	}

	e.expect(e.do("GET", "/v1/usage", adminToken, "", nil), 403, "forbidden")
	e.expect(e.do("GET", "/v1/usage", "", "", nil), 401, "unauthorized")
	e.expect(e.do("POST", "/v1/usage", e.tokenA, "{}", nil), 405, "method_not_allowed")
}

func TestContractSTORE210_ListAndGetTenants(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	r := e.do("GET", "/v1/admin/tenants", adminToken, "", nil)
	contractNotImplemented(t, r, "GET /v1/admin/tenants")
	e.expect(r, 200, "")

	var list struct {
		Tenants []json.RawMessage `json:"tenants"`
	}
	decodeJSON(t, r, &list)
	if len(list.Tenants) != 2 {
		t.Fatalf("tenants = %d, want 2 (a and b): %s", len(list.Tenants), r.body)
	}
	for i, raw := range list.Tenants {
		m := fieldsOf(t, raw)
		requireFields(t, "TenantSummary", m, "id", "name", "quota", "usage")
		requireFields(t, "TenantSummary.quota", fieldsOf(t, m["quota"]), "max_keys", "max_bytes", "version")
		requireFields(t, "TenantSummary.usage", fieldsOf(t, m["usage"]), "keys", "bytes")
		var id int64
		_ = json.Unmarshal(m["id"], &id)
		if id != int64(i+1) {
			t.Errorf("tenants not sorted by id: position %d has id %d", i, id)
		}
	}

	one := e.do("GET", "/v1/admin/tenants/1", adminToken, "", nil)
	contractNotImplemented(t, one, "GET /v1/admin/tenants/{id}")
	e.expect(one, 200, "")
	var raw json.RawMessage
	decodeJSON(t, one, &raw)
	requireFields(t, "TenantSummary", fieldsOf(t, raw), "id", "name", "quota", "usage")

	e.expect(e.do("GET", "/v1/admin/tenants/abc", adminToken, "", nil), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/admin/tenants/0", adminToken, "", nil), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/admin/tenants/999", adminToken, "", nil), 404, "not_found")
	for _, path := range []string{"/v1/admin/tenants", "/v1/admin/tenants/1"} {
		e.expect(e.do("GET", path, e.tokenA, "", nil), 403, "forbidden")
		e.expect(e.do("GET", path, "", "", nil), 401, "unauthorized")
	}
}

func TestContractSTORE210_QuotaCAS(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	path := "/v1/admin/tenants/1/quota"

	g := e.do("GET", path, adminToken, "", nil)
	contractNotImplemented(t, g, "GET /v1/admin/tenants/{id}/quota")
	e.expect(g, 200, "")
	if g.header.Get("ETag") != `"0"` {
		t.Fatalf(`default quota ETag = %q, want "0"`, g.header.Get("ETag"))
	}
	var q struct {
		MaxKeys  *int64 `json:"max_keys"`
		MaxBytes *int64 `json:"max_bytes"`
		Version  int64  `json:"version"`
	}
	decodeJSON(t, g, &q)
	if q.MaxKeys == nil || *q.MaxKeys != 1000 || q.MaxBytes == nil || *q.MaxBytes != 1048576 || q.Version != 0 {
		t.Fatalf("default quota = %s, want 1000 / 1048576 / version 0", g.body)
	}

	body := `{"max_keys": 5, "max_bytes": null}`
	e.expect(e.do("PUT", path, adminToken, body, nil), 428, "precondition_required")

	p := e.do("PUT", path, adminToken, body, map[string]string{"If-Match": `"0"`})
	e.expect(p, 200, "")
	if p.header.Get("ETag") != `"1"` {
		t.Fatalf(`ETag after the first edit = %q, want "1"`, p.header.Get("ETag"))
	}
	decodeJSON(t, p, &q)
	if q.MaxKeys == nil || *q.MaxKeys != 5 || q.MaxBytes != nil || q.Version != 1 {
		t.Fatalf("quota after edit = %s, want 5 / null / version 1", p.body)
	}

	e.expect(e.do("PUT", path, adminToken, body, map[string]string{"If-Match": `"0"`}), 412, "precondition_failed")
	for name, bad := range map[string]string{
		"negative":      `{"max_keys": -1, "max_bytes": 10}`,
		"decimal":       `{"max_keys": 1.5, "max_bytes": 10}`,
		"text":          `{"max_keys": "ten", "max_bytes": 10}`,
		"missing field": `{"max_keys": 10}`,
		"unknown field": `{"max_keys": 10, "max_bytes": 10, "max_values": 1}`,
		"not JSON":      `{nope`,
	} {
		r := e.do("PUT", path, adminToken, bad, map[string]string{"If-Match": `"1"`})
		if r.status != 400 {
			t.Errorf("%s: status %d, want 400 (%s)", name, r.status, r.body)
		}
	}
	e.expect(e.do("PUT", "/v1/admin/tenants/999/quota", adminToken, body, map[string]string{"If-Match": `"0"`}), 404, "not_found")
	e.expect(e.do("PUT", path, e.tokenA, body, map[string]string{"If-Match": `"1"`}), 403, "forbidden")
	e.expect(e.do("GET", path, "", "", nil), 401, "unauthorized")
}

func TestContractSTORE210_Flags(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	for name, tok := range map[string]string{"tenant": e.tokenA, "admin": adminToken} {
		r := e.do("GET", "/v1/flags", tok, "", nil)
		contractNotImplemented(t, r, "GET /v1/flags")
		e.expect(r, 200, "")
		var f struct {
			QuotaEnforcement *struct {
				Enabled bool  `json:"enabled"`
				Version int64 `json:"version"`
			} `json:"quota_enforcement"`
		}
		decodeJSON(t, r, &f)
		if f.QuotaEnforcement == nil || f.QuotaEnforcement.Enabled || f.QuotaEnforcement.Version != 0 {
			t.Fatalf("%s: flags = %s, want quota_enforcement off at version 0", name, r.body)
		}
	}
	e.expect(e.do("GET", "/v1/flags", "", "", nil), 401, "unauthorized")

	path := "/v1/admin/flags/quota_enforcement"
	e.expect(e.do("PUT", path, adminToken, `{"enabled": true}`, nil), 428, "precondition_required")
	p := e.do("PUT", path, adminToken, `{"enabled": true}`, map[string]string{"If-Match": `"0"`})
	contractNotImplemented(t, p, "PUT /v1/admin/flags/quota_enforcement")
	e.expect(p, 200, "")
	if p.header.Get("ETag") != `"1"` || p.body == "" {
		t.Fatalf("flag PUT: ETag %q body %s", p.header.Get("ETag"), p.body)
	}
	var flag struct {
		Enabled bool  `json:"enabled"`
		Version int64 `json:"version"`
	}
	decodeJSON(t, p, &flag)
	if !flag.Enabled || flag.Version != 1 {
		t.Fatalf("flag after PUT = %s", p.body)
	}
	e.expect(e.do("PUT", path, adminToken, `{"enabled": false}`, map[string]string{"If-Match": `"0"`}), 412, "precondition_failed")
	e.expect(e.do("PUT", path, adminToken, `{"enabled": "yes"}`, map[string]string{"If-Match": `"1"`}), 400, "invalid_request")
	e.expect(e.do("PUT", path, e.tokenA, `{"enabled": false}`, map[string]string{"If-Match": `"1"`}), 403, "forbidden")
}

// The 403 quota_exceeded envelope carries details with dimension/used/limit/requested.
func TestContractSTORE210_QuotaExceededEnvelope(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	f := e.do("PUT", "/v1/admin/flags/quota_enforcement", adminToken, `{"enabled": true}`, map[string]string{"If-Match": `"0"`})
	contractNotImplemented(t, f, "PUT /v1/admin/flags/quota_enforcement")
	e.expect(f, 200, "")
	q := e.do("PUT", "/v1/admin/tenants/1/quota", adminToken, `{"max_keys": 1, "max_bytes": null}`, map[string]string{"If-Match": `"0"`})
	e.expect(q, 200, "")

	e.put(e.tokenA, "first", `{"value":1}`)
	r := e.do("PUT", "/v1/keys/second", e.tokenA, `{"value":2}`, nil)
	e.expect(r, 403, "quota_exceeded")
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details *struct {
				Dimension string `json:"dimension"`
				Used      *int64 `json:"used"`
				Limit     *int64 `json:"limit"`
				Requested *int64 `json:"requested"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeJSON(t, r, &env)
	d := env.Error.Details
	if d == nil || d.Dimension != "keys" || d.Used == nil || *d.Used != 1 || d.Limit == nil || *d.Limit != 1 || d.Requested == nil || *d.Requested != 1 {
		t.Fatalf("quota_exceeded details = %s", r.body)
	}
	for _, want := range []string{fmt.Sprint(*d.Used), fmt.Sprint(*d.Limit)} {
		if !containsDigitRun(env.Error.Message, want) {
			t.Errorf("message %q does not contain %s", env.Error.Message, want)
		}
	}
}

// containsDigitRun reports whether s contains n as a separate number.
func containsDigitRun(s, n string) bool {
	for i := 0; i+len(n) <= len(s); i++ {
		if s[i:i+len(n)] != n {
			continue
		}
		before := i == 0 || s[i-1] < '0' || s[i-1] > '9'
		after := i+len(n) == len(s) || s[i+len(n)] < '0' || s[i+len(n)] > '9'
		if before && after {
			return true
		}
	}
	return false
}
