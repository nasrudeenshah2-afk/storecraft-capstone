package api

// Acceptance tests for the STORE-210 API criteria (docs/capstone/spec.md
// section 6). Written in step B5 BEFORE the implementation: real HTTP, real
// temp SQLite files, real concurrency, no mocks. Every test name contains
// "Quota" so `go test -race -count=20 -run Quota ./internal/...` runs them all.
// The AC number is in each test name (AC01 = AC-API-1, ...).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/store"
)

// esc turns every X into a backslash, to write JSON escapes such as
// \u003c in Go source without an editor turning them into characters.
func esc(s string) string { return strings.ReplaceAll(s, "X", `\`) }

// lim is a limit for setQuota; nil means unlimited.
func lim(n int64) *int64 { return &n }

// setEnforcement turns quota_enforcement on or off through the admin API.
func (e *env) setEnforcement(on bool) {
	e.t.Helper()
	g := e.do("GET", "/v1/flags", adminToken, "", nil)
	contractNotImplemented(e.t, g, "GET /v1/flags")
	e.expect(g, 200, "")
	var f struct {
		QuotaEnforcement struct {
			Enabled bool  `json:"enabled"`
			Version int64 `json:"version"`
		} `json:"quota_enforcement"`
	}
	decodeJSON(e.t, g, &f)
	p := e.do("PUT", "/v1/admin/flags/quota_enforcement", adminToken, fmt.Sprintf(`{"enabled":%t}`, on),
		map[string]string{"If-Match": etag(f.QuotaEnforcement.Version)})
	contractNotImplemented(e.t, p, "PUT /v1/admin/flags/quota_enforcement")
	e.expect(p, 200, "")
}

type quotaResp struct {
	MaxKeys  *int64 `json:"max_keys"`
	MaxBytes *int64 `json:"max_bytes"`
	Version  int64  `json:"version"`
}

// getQuota reads a tenant's quota through the admin API.
func (e *env) getQuota(tenantID int64) quotaResp {
	e.t.Helper()
	r := e.do("GET", fmt.Sprintf("/v1/admin/tenants/%d/quota", tenantID), adminToken, "", nil)
	contractNotImplemented(e.t, r, "GET /v1/admin/tenants/{id}/quota")
	e.expect(r, 200, "")
	var q quotaResp
	decodeJSON(e.t, r, &q)
	return q
}

// setQuota changes a tenant's quota through the admin API (CAS on the current
// version) and returns the new quota.
func (e *env) setQuota(tenantID int64, maxKeys, maxBytes *int64) quotaResp {
	e.t.Helper()
	cur := e.getQuota(tenantID)
	body, _ := json.Marshal(map[string]*int64{"max_keys": maxKeys, "max_bytes": maxBytes})
	r := e.do("PUT", fmt.Sprintf("/v1/admin/tenants/%d/quota", tenantID), adminToken, string(body),
		map[string]string{"If-Match": etag(cur.Version)})
	contractNotImplemented(e.t, r, "PUT /v1/admin/tenants/{id}/quota")
	e.expect(r, 200, "")
	var q quotaResp
	decodeJSON(e.t, r, &q)
	return q
}

type usageResp struct {
	TenantID int64 `json:"tenant_id"`
	Keys     struct {
		Used  int64  `json:"used"`
		Limit *int64 `json:"limit"`
	} `json:"keys"`
	Bytes struct {
		Used  int64  `json:"used"`
		Limit *int64 `json:"limit"`
	} `json:"bytes"`
	Seq int64 `json:"seq"`
}

// String shows the values (limits dereferenced), so two readings can be
// compared by value; comparing the structs would compare limit pointers.
func (u usageResp) String() string {
	lim := func(p *int64) string {
		if p == nil {
			return "unlimited"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("tenant %d: %d of %s keys, %d of %s bytes, seq %d",
		u.TenantID, u.Keys.Used, lim(u.Keys.Limit), u.Bytes.Used, lim(u.Bytes.Limit), u.Seq)
}

// usage is GET /v1/usage for a tenant token.
func (e *env) usage(token string) usageResp {
	e.t.Helper()
	r := e.do("GET", "/v1/usage", token, "", nil)
	contractNotImplemented(e.t, r, "GET /v1/usage")
	e.expect(r, 200, "")
	var u usageResp
	decodeJSON(e.t, r, &u)
	return u
}

// rawDBFile opens a separate connection to the server's SQLite file, to check
// the stored data independently of the code under test.
func rawDBFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// realUsage is the tenant's usage computed directly from kv: COUNT(*) and the
// sum of stored value bytes.
func realUsage(t *testing.T, dbPath string, tenantID int64) (keys, bytes int64) {
	t.Helper()
	err := rawDBFile(t, dbPath).QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(length(CAST(value AS BLOB))), 0) FROM kv WHERE tenant_id = ?`, tenantID).
		Scan(&keys, &bytes)
	if err != nil {
		t.Fatal(err)
	}
	return keys, bytes
}

// rowCount counts the rows of a table through a separate connection.
func rowCount(t *testing.T, dbPath, table string) int64 {
	t.Helper()
	var n int64
	if err := rawDBFile(t, dbPath).QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireUsageMatchesData checks GET /v1/usage against the data itself.
func (e *env) requireUsageMatchesData(token string, tenantID int64) usageResp {
	e.t.Helper()
	u := e.usage(token)
	keys, bytes := realUsage(e.t, e.dbPath, tenantID)
	if u.Keys.Used != keys || u.Bytes.Used != bytes {
		e.t.Fatalf("usage %d keys / %d bytes, but the data has %d keys / %d bytes", u.Keys.Used, u.Bytes.Used, keys, bytes)
	}
	return u
}

type quotaDetails struct {
	Dimension string `json:"dimension"`
	Used      int64  `json:"used"`
	Limit     int64  `json:"limit"`
	Requested int64  `json:"requested"`
}

// requireQuotaExceeded checks a 403 quota_exceeded response and returns its details.
func (e *env) requireQuotaExceeded(r resp) quotaDetails {
	e.t.Helper()
	e.expect(r, 403, "quota_exceeded")
	var env struct {
		Error struct {
			Message string        `json:"message"`
			Details *quotaDetails `json:"details"`
		} `json:"error"`
	}
	decodeJSON(e.t, r, &env)
	if env.Error.Details == nil {
		e.t.Fatalf("quota_exceeded without details: %s", r.body)
	}
	d := *env.Error.Details
	for _, n := range []int64{d.Used, d.Limit} {
		if !containsDigitRun(env.Error.Message, fmt.Sprint(n)) {
			e.t.Errorf("message %q does not contain %d", env.Error.Message, n)
		}
	}
	return d
}

// result is one finished request made from a goroutine (no t.Fatal there).
type result struct {
	status int
	code   string // error envelope code, if any
	header http.Header
	err    error
}

// send performs one request without touching t (safe in goroutines).
func send(base, method, path, token, body string, hdr map[string]string) result {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return result{err: err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{err: err}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := result{status: res.StatusCode, header: res.Header}
	if res.StatusCode >= 300 {
		var env struct{ Error struct{ Code string } }
		_ = json.Unmarshal(b, &env)
		out.code = env.Error.Code
	}
	return out
}

// concurrently runs n requests at the same moment and returns their results.
func concurrently(n int, req func(i int) result) []result {
	out := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out[i] = req(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return out
}

// tally counts results by "status code" (for example "201", "403 quota_exceeded").
func tally(rs []result) map[string]int {
	m := map[string]int{}
	for _, r := range rs {
		switch {
		case r.err != nil:
			m["error "+r.err.Error()]++
		case r.code != "":
			m[fmt.Sprintf("%d %s", r.status, r.code)]++
		default:
			m[fmt.Sprint(r.status)]++
		}
	}
	return m
}

// AC-API-1: at max_keys, creating a key is rejected with exact details and
// nothing is written; overwriting an existing key still works.
func TestQuota_AC01_KeyLimit(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(3), nil)
	for i := 0; i < 3; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%d", i), `{"value":"x"}`)
	}
	before := e.usage(e.tokenA)
	changesBefore := rowCount(t, e.dbPath, "changes")

	for _, hdr := range []map[string]string{nil, {"If-None-Match": "*"}} {
		d := e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":"x"}`, hdr))
		if d != (quotaDetails{Dimension: "keys", Used: 3, Limit: 3, Requested: 1}) {
			t.Fatalf("details = %+v, want keys 3/3 requested 1", d)
		}
	}
	e.expect(e.do("GET", "/v1/keys/k3", e.tokenA, "", nil), 404, "not_found")
	if n := rowCount(t, e.dbPath, "changes"); n != changesBefore {
		t.Fatalf("a rejected write added %d change rows", n-changesBefore)
	}
	if after := e.usage(e.tokenA); after.String() != before.String() {
		t.Fatalf("usage changed by a rejected write: %+v -> %+v", before, after)
	}

	e.expect(e.do("PUT", "/v1/keys/k0", e.tokenA, `{"value":"overwritten"}`, nil), 200, "")
	e.put(e.tokenB, "other-tenant", `{"value":"x"}`) // another tenant is unaffected
	e.requireUsageMatchesData(e.tokenA, 1)
}

// AC-API-2: the byte limit counts the overwrite delta; shrinking overwrites and
// deletes always work; when both limits break, the dimension is "keys".
func TestQuota_AC02_ByteLimitAndOverwriteDelta(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, nil, lim(20))

	e.put(e.tokenA, "k1", `{"value":"0123456789"}`) // 12 bytes
	d := e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k2", e.tokenA, `{"value":"abcdefgh"}`, nil))
	if d != (quotaDetails{Dimension: "bytes", Used: 12, Limit: 20, Requested: 10}) {
		t.Fatalf("details = %+v, want bytes 12/20 requested 10", d)
	}
	e.expect(e.do("GET", "/v1/keys/k2", e.tokenA, "", nil), 404, "not_found")

	// 12 -> 18 bytes: the new value (18) is larger than the free space (8),
	// but only the difference (6) counts.
	e.expect(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"0123456789abcdef"}`, nil), 200, "")
	if u := e.requireUsageMatchesData(e.tokenA, 1); u.Bytes.Used != 18 {
		t.Fatalf("bytes.used = %d, want 18", u.Bytes.Used)
	}

	// Over quota after lowering the limit: shrinking and equal overwrites work,
	// a growing one does not.
	e.setQuota(1, nil, lim(10))
	e.expect(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"012345678901"}`, nil), 200, "") // 14 bytes
	e.expect(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"abcdefghijkl"}`, nil), 200, "") // 14 bytes
	d = e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"abcdefghijklm"}`, nil))
	if d != (quotaDetails{Dimension: "bytes", Used: 14, Limit: 10, Requested: 1}) {
		t.Fatalf("details = %+v, want bytes 14/10 requested 1", d)
	}
	e.expect(e.do("DELETE", "/v1/keys/k1", e.tokenA, "", nil), 204, "")
	if u := e.requireUsageMatchesData(e.tokenA, 1); u.Bytes.Used != 0 || u.Keys.Used != 0 {
		t.Fatalf("after delete usage = %+v, want 0/0", u)
	}

	// Both limits would break: keys wins.
	e.setQuota(1, lim(1), lim(5))
	e.put(e.tokenA, "small", `{"value":"a"}`) // 3 bytes, 1 key
	d = e.requireQuotaExceeded(e.do("PUT", "/v1/keys/big", e.tokenA, `{"value":"abcdefgh"}`, nil))
	if d.Dimension != "keys" {
		t.Fatalf("both limits broken: dimension = %q, want keys", d.Dimension)
	}
}

// AC-API-3 (keys): 100 concurrent writers against a tenant 10 keys below its
// limit; exactly 10 creates succeed and usage equals the data.
func TestQuota_AC03_ConcurrentCreatesNearKeyLimit(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(50), nil)
	for i := 0; i < 40; i++ {
		e.put(e.tokenA, fmt.Sprintf("pre%02d", i), `{"value":1}`)
	}
	rs := concurrently(100, func(i int) result {
		return send(e.url, "PUT", fmt.Sprintf("/v1/keys/new%03d", i), e.tokenA, `{"value":1}`, nil)
	})
	got := tally(rs)
	if got["201"] != 10 || got["403 quota_exceeded"] != 90 || len(got) != 2 {
		t.Fatalf("results = %v, want exactly 10 x 201 and 90 x 403 quota_exceeded", got)
	}
	u := e.requireUsageMatchesData(e.tokenA, 1)
	if u.Keys.Used != 50 {
		t.Fatalf("keys.used = %d, want exactly 50", u.Keys.Used)
	}
}

// AC-API-3 (bytes): 100 concurrent writers near max_bytes; exactly as many
// succeed as fit, and usage equals the data.
func TestQuota_AC03_ConcurrentCreatesNearByteLimit(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, nil, lim(1000))
	for i := 0; i < 9; i++ {
		e.put(e.tokenA, fmt.Sprintf("pre%d", i), `{"value":"`+strings.Repeat("x", 98)+`"}`) // 100 bytes each
	}
	rs := concurrently(100, func(i int) result {
		return send(e.url, "PUT", fmt.Sprintf("/v1/keys/new%03d", i), e.tokenA, `{"value":"12345678"}`, nil) // 10 bytes
	})
	got := tally(rs)
	if got["201"] != 10 || got["403 quota_exceeded"] != 90 || len(got) != 2 {
		t.Fatalf("results = %v, want exactly 10 x 201 and 90 x 403 quota_exceeded", got)
	}
	u := e.requireUsageMatchesData(e.tokenA, 1)
	if u.Bytes.Used != 1000 {
		t.Fatalf("bytes.used = %d, want exactly 1000", u.Bytes.Used)
	}
}

// AC-API-3 (mixed): creates, growing and shrinking overwrites and deletes at
// the same moment never push usage over a limit and never make it drift.
func TestQuota_AC03_ConcurrentMixedWritesKeepInvariant(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(30), lim(600))
	for i := 0; i < 25; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%02d", i), `{"value":"`+strings.Repeat("y", 18)+`"}`) // 20 bytes each = 500
	}
	rs := concurrently(100, func(i int) result {
		key := fmt.Sprintf("k%02d", i%40) // 25 existing keys and 15 new ones
		switch i % 4 {
		case 0:
			return send(e.url, "DELETE", "/v1/keys/"+key, e.tokenA, "", nil)
		case 1:
			return send(e.url, "PUT", "/v1/keys/"+key, e.tokenA, `{"value":"s"}`, nil) // small
		default:
			return send(e.url, "PUT", "/v1/keys/"+key, e.tokenA, `{"value":"`+strings.Repeat("z", 38)+`"}`, nil) // 40 bytes
		}
	})
	for _, r := range rs {
		ok := r.err == nil && (r.status == 200 || r.status == 201 || r.status == 204 ||
			(r.status == 404 && r.code == "not_found") || (r.status == 403 && r.code == "quota_exceeded"))
		if !ok {
			t.Fatalf("unexpected result: %+v (all: %v)", r, tally(rs))
		}
	}
	u := e.requireUsageMatchesData(e.tokenA, 1)
	if u.Keys.Used > 30 || u.Bytes.Used > 600 {
		t.Fatalf("usage %d keys / %d bytes exceeds the quota 30 / 600", u.Keys.Used, u.Bytes.Used)
	}
}

// AC-API-4: a tenant reads only its own usage; admins read any tenant; tenant
// tokens are refused on admin routes.
func TestQuota_AC04_ReadUsageIsolatedPerTenant(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "a1", `{"value":"aa"}`)
	e.put(e.tokenA, "a2", `{"value":"aa"}`)
	e.put(e.tokenB, "b1", `{"value":"b"}`)

	ua := e.requireUsageMatchesData(e.tokenA, 1)
	ub := e.requireUsageMatchesData(e.tokenB, 2)
	if ua.TenantID != 1 || ua.Keys.Used != 2 || ub.TenantID != 2 || ub.Keys.Used != 1 {
		t.Fatalf("usage a=%+v b=%+v", ua, ub)
	}
	// There is no way to name another tenant.
	r := e.do("GET", "/v1/usage?tenant_id=2", e.tokenA, "", nil)
	e.expect(r, 200, "")
	var u usageResp
	decodeJSON(t, r, &u)
	if u.TenantID != 1 || u.Keys.Used != 2 {
		t.Fatalf("a tenant could read another tenant's usage: %s", r.body)
	}
	// Seq is where the numbers were read; it equals the latest change.
	if latest, _ := e.st.LatestSeq(context.Background()); u.Seq != latest {
		t.Fatalf("usage seq = %d, want %d", u.Seq, latest)
	}

	one := e.do("GET", "/v1/admin/tenants/2", adminToken, "", nil)
	contractNotImplemented(t, one, "GET /v1/admin/tenants/{id}")
	e.expect(one, 200, "")
	var ts struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Usage struct {
			Keys  int64 `json:"keys"`
			Bytes int64 `json:"bytes"`
		} `json:"usage"`
	}
	decodeJSON(t, one, &ts)
	if ts.ID != 2 || ts.Name != "b" || ts.Usage.Keys != ub.Keys.Used || ts.Usage.Bytes != ub.Bytes.Used {
		t.Fatalf("admin view of tenant 2 = %s, want usage %+v", one.body, ub)
	}

	for _, path := range []string{"/v1/admin/tenants", "/v1/admin/tenants/1", "/v1/admin/tenants/1/quota", "/v1/admin/tenants/2/quota"} {
		e.expect(e.do("GET", path, e.tokenA, "", nil), 403, "forbidden")
		e.expect(e.do("GET", path, "", "", nil), 401, "unauthorized")
		e.expect(e.do("GET", path, "wrong-token", "", nil), 401, "unauthorized")
	}
	e.expect(e.do("PUT", "/v1/admin/tenants/2/quota", e.tokenA, `{"max_keys":null,"max_bytes":null}`, map[string]string{"If-Match": `"0"`}), 403, "forbidden")
	e.expect(e.do("PUT", "/v1/admin/flags/quota_enforcement", e.tokenB, `{"enabled":false}`, map[string]string{"If-Match": `"0"`}), 403, "forbidden")
}

// AC-API-5: quota edits use CAS; a quota may be lowered below usage; two admins
// saving at the same moment produce exactly one winner.
func TestQuota_AC05_QuotaEditCAS(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	path := "/v1/admin/tenants/1/quota"
	q := e.getQuota(1)
	r := e.do("PUT", path, adminToken, `{"max_keys":5,"max_bytes":null}`, map[string]string{"If-Match": etag(q.Version)})
	contractNotImplemented(t, r, "PUT /v1/admin/tenants/{id}/quota")
	e.expect(r, 200, "")
	if r.header.Get("ETag") != etag(q.Version+1) {
		t.Fatalf("ETag = %q, want %q", r.header.Get("ETag"), etag(q.Version+1))
	}
	before := e.getQuota(1)
	e.expect(e.do("PUT", path, adminToken, `{"max_keys":7,"max_bytes":null}`, map[string]string{"If-Match": etag(q.Version)}), 412, "precondition_failed")
	e.expect(e.do("PUT", path, adminToken, `{"max_keys":7,"max_bytes":null}`, nil), 428, "precondition_required")
	if after := e.getQuota(1); *after.MaxKeys != *before.MaxKeys || after.Version != before.Version {
		t.Fatalf("a rejected edit changed the quota: %+v -> %+v", before, after)
	}

	// Two admins at the same moment, both from the same version.
	v := before.Version
	rs := concurrently(10, func(i int) result {
		return send(e.url, "PUT", path, adminToken, fmt.Sprintf(`{"max_keys":%d,"max_bytes":null}`, 100+i), map[string]string{"If-Match": etag(v)})
	})
	if got := tally(rs); got["200"] != 1 || got["412 precondition_failed"] != 9 {
		t.Fatalf("concurrent edits = %v, want exactly one 200 and nine 412", got)
	}
	if after := e.getQuota(1); after.Version != v+1 {
		t.Fatalf("version after the race = %d, want %d", after.Version, v+1)
	}

	// Lower below usage: growing writes fail, deletes and shrinking overwrites work.
	e.setEnforcement(true)
	for i := 0; i < 4; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%d", i), `{"value":"abcdef"}`)
	}
	e.setQuota(1, lim(2), lim(10))
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/new", e.tokenA, `{"value":1}`, nil))
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k0", e.tokenA, `{"value":"abcdefg"}`, nil))
	e.expect(e.do("PUT", "/v1/keys/k0", e.tokenA, `{"value":"a"}`, nil), 200, "")
	e.expect(e.do("DELETE", "/v1/keys/k1", e.tokenA, "", nil), 204, "")
	e.requireUsageMatchesData(e.tokenA, 1)

	// null means unlimited.
	e.setQuota(1, nil, nil)
	u := e.usage(e.tokenA)
	if u.Keys.Limit != nil || u.Bytes.Limit != nil {
		t.Fatalf("unlimited quota reported as %+v", u)
	}
	e.put(e.tokenA, "now-allowed", `{"value":1}`)
}

// AC-API-6: tenants without a quota record get the default quota at version 0.
func TestQuota_AC06_DefaultQuota(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	if _, err := e.st.CreateTenant(context.Background(), "late", "hash-of-a-synthetic-token"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 3} {
		q := e.getQuota(id)
		if q.MaxKeys == nil || *q.MaxKeys != 1000 || q.MaxBytes == nil || *q.MaxBytes != 1048576 || q.Version != 0 {
			t.Fatalf("tenant %d default quota = %+v", id, q)
		}
	}
	u := e.usage(e.tokenB)
	if u.Keys.Limit == nil || *u.Keys.Limit != 1000 || u.Bytes.Limit == nil || *u.Bytes.Limit != 1048576 {
		t.Fatalf("default limits in usage = %+v", u)
	}
	r := e.do("PUT", "/v1/admin/tenants/3/quota", adminToken, `{"max_keys":1,"max_bytes":1}`, map[string]string{"If-Match": `"0"`})
	e.expect(r, 200, "")
	if r.header.Get("ETag") != `"1"` {
		t.Fatalf("first edit ETag = %q", r.header.Get("ETag"))
	}
}

// AC-API-7: idempotent replays are never counted or rejected; a rejected
// write stores no idempotency record, so the same request succeeds once after
// the quota is raised.
func TestQuota_AC07_IdempotencyNeverDoubleCounts(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(2), nil)
	idem := map[string]string{"Idempotency-Key": "create-k1"}
	e.expect(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"v"}`, idem), 201, "")
	e.put(e.tokenA, "k2", `{"value":"v"}`) // now at the limit
	u1 := e.requireUsageMatchesData(e.tokenA, 1)
	r := e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"v"}`, idem) // replay at the limit
	e.expect(r, 201, "")
	if r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay not marked: %v", r.header)
	}
	if u2 := e.requireUsageMatchesData(e.tokenA, 1); u2.String() != u1.String() {
		t.Fatalf("a replay changed usage: %+v -> %+v", u1, u2)
	}

	// Rejected: no record is stored.
	idem3 := map[string]string{"Idempotency-Key": "create-k3"}
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":"v"}`, idem3))
	if n := rowCount(t, e.dbPath, "idempotency_keys"); n != 1 {
		t.Fatalf("idempotency records = %d, want 1 (a 403 must store none)", n)
	}
	e.setQuota(1, lim(3), nil)
	r = e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":"v"}`, idem3)
	e.expect(r, 201, "")
	if r.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("the first successful attempt was reported as a replay")
	}
	e.expect(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":"v"}`, idem3), 201, "")
	if u := e.requireUsageMatchesData(e.tokenA, 1); u.Keys.Used != 3 {
		t.Fatalf("keys.used = %d, want 3 (counted once)", u.Keys.Used)
	}

	// The same idempotent request 20 times at the same moment, with room for
	// exactly one key: one write, 19 replays, counted once.
	e.setQuota(1, lim(4), nil)
	same := map[string]string{"Idempotency-Key": "burst"}
	rs := concurrently(20, func(int) result {
		return send(e.url, "PUT", "/v1/keys/burst", e.tokenA, `{"value":"b"}`, same)
	})
	replays := 0
	for _, r := range rs {
		if r.err != nil || r.status != 201 {
			t.Fatalf("burst result %+v (all %v)", r, tally(rs))
		}
		if r.header.Get("Idempotent-Replayed") == "true" {
			replays++
		}
	}
	if replays != 19 {
		t.Fatalf("replays = %d, want 19", replays)
	}
	if u := e.requireUsageMatchesData(e.tokenA, 1); u.Keys.Used != 4 {
		t.Fatalf("keys.used = %d, want 4", u.Keys.Used)
	}
}

// serveExisting starts a server on an existing database file (a restart).
func serveExisting(t *testing.T, old *env) *env {
	t.Helper()
	st, err := store.Open(context.Background(), old.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, Options{AdminToken: adminToken, PollTimeout: 300 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(srv.Close)
	return &env{t: t, url: srv.URL, dbPath: old.dbPath, st: st, srv: srv, tokenA: old.tokenA, tokenB: old.tokenB}
}

// AC-API-8: usage, quotas and the flag survive a restart.
func TestQuota_AC08_SurvivesRestart(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(3), lim(500))
	e.put(e.tokenA, "k1", `{"value":"hello"}`)
	e.put(e.tokenA, "k2", `{"value":[1,2,3]}`)
	u1, q1 := e.usage(e.tokenA), e.getQuota(1)
	f1 := e.do("GET", "/v1/flags", e.tokenA, "", nil).body

	e.srv.Close()
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := serveExisting(t, e)
	u2, q2 := e2.usage(e2.tokenA), e2.getQuota(1)
	if u1.String() != u2.String() || *q1.MaxKeys != *q2.MaxKeys || *q1.MaxBytes != *q2.MaxBytes || q1.Version != q2.Version {
		t.Fatalf("after restart usage %+v -> %+v, quota %+v -> %+v", u1, u2, q1, q2)
	}
	if f2 := e2.do("GET", "/v1/flags", e2.tokenA, "", nil).body; f1 != f2 {
		t.Fatalf("flags after restart %s, before %s", f2, f1)
	}
	e2.put(e2.tokenA, "k3", `{"value":1}`)
	e2.requireQuotaExceeded(e2.do("PUT", "/v1/keys/k4", e2.tokenA, `{"value":1}`, nil))
	e2.requireUsageMatchesData(e2.tokenA, 1)
}

// AC-API-9 (single node): values are stored in canonical form and usage counts
// canonical UTF-8 bytes.
func TestQuota_AC09_CanonicalStorageAndByteCount(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	cases := []struct{ key, sent, canonical string }{
		{"ws", `{ "b" : 1 ,  "a" : [ 1 , 2.50 , 1e3 ] }`, `{"b":1,"a":[1,2.50,1e3]}`},
		{"html", `"<b>Tom & Jerry</b>"`, `"<b>Tom & Jerry</b>"`},
		{"escaped", esc(`"Xu003cbXu003e Xu0026 Xu00e9"`), `"<b> & é"`},
		{"utf8", `"héllo wörld €"`, `"héllo wörld €"`},
	}
	var total int64
	for _, c := range cases {
		e.put(e.tokenA, c.key, `{"value":`+c.sent+`}`)
		var stored []byte
		if err := rawDBFile(t, e.dbPath).QueryRow(`SELECT value FROM kv WHERE tenant_id = 1 AND key = ?`, c.key).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if string(stored) != c.canonical {
			t.Errorf("%s: stored %q, want canonical %q", c.key, stored, c.canonical)
		}
		r := e.do("GET", "/v1/keys/"+c.key, e.tokenA, "", nil)
		if want := `"value":` + c.canonical; !strings.Contains(r.body, want) {
			t.Errorf("%s: GET body %s does not contain %s", c.key, r.body, want)
		}
		total += int64(len(c.canonical)) // bytes, not characters
	}
	if u := e.requireUsageMatchesData(e.tokenA, 1); u.Bytes.Used != total {
		t.Fatalf("bytes.used = %d, want %d (canonical UTF-8 bytes)", u.Bytes.Used, total)
	}
	// The change feed carries the same canonical bytes.
	for _, c := range e.changes(e.tokenA, "?since=0").Changes {
		if c.Key == "escaped" && string(c.Value) != `"<b> & é"` {
			t.Errorf("change feed value %s, want canonical", c.Value)
		}
	}
}

// AC-API-11: with the flag off nothing is rejected but usage is still counted;
// the very next write obeys a toggle; flag edits use CAS.
func TestQuota_AC11_FlagOffCountsButDoesNotReject(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setQuota(1, lim(1), lim(5))
	e.put(e.tokenA, "k1", `{"value":"abcdef"}`)
	e.put(e.tokenA, "k2", `{"value":"abcdef"}`) // over both limits, flag is off
	u := e.requireUsageMatchesData(e.tokenA, 1)
	if u.Keys.Used != 2 || *u.Keys.Limit != 1 || u.Bytes.Used != 16 {
		t.Fatalf("usage with the flag off = %+v", u)
	}

	e.setEnforcement(true)
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":1}`, nil))
	e.setEnforcement(false)
	e.expect(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":1}`, nil), 201, "")

	path := "/v1/admin/flags/quota_enforcement"
	e.expect(e.do("PUT", path, adminToken, `{"enabled":true}`, map[string]string{"If-Match": `"0"`}), 412, "precondition_failed")
	e.expect(e.do("PUT", path, adminToken, `{"enabled":true}`, nil), 428, "precondition_required")
	e.expect(e.do("PUT", path, adminToken, `{"enabled":true,"extra":1}`, map[string]string{"If-Match": `"2"`}), 400, "invalid_request")
	e.expect(e.do("PUT", path, adminToken, `{}`, map[string]string{"If-Match": `"2"`}), 400, "invalid_request")
}

// AC-API-11 (under load): toggling the flag several times during a concurrent
// load produces no failure other than expected quota_exceeded responses, and
// every write is correctly counted.
func TestQuota_AC11_ToggleUnderConcurrentLoad(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setQuota(1, lim(20), nil)
	for i := 0; i < 15; i++ {
		e.put(e.tokenA, fmt.Sprintf("full%02d", i), `{"value":1}`)
	}

	stop := make(chan struct{})
	var mu sync.Mutex
	var bad []result
	counts := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				path := fmt.Sprintf("/v1/keys/w%02d-%d", w, i%4)
				var r result
				if i%3 == 2 {
					r = send(e.url, "DELETE", path, e.tokenA, "", nil)
				} else {
					r = send(e.url, "PUT", path, e.tokenA, `{"value":1}`, nil)
				}
				ok := r.err == nil && (r.status == 200 || r.status == 201 || r.status == 204 ||
					(r.status == 404 && r.code == "not_found") || (r.status == 403 && r.code == "quota_exceeded"))
				mu.Lock()
				for k, v := range tally([]result{r}) {
					counts[k] += v
				}
				if !ok {
					bad = append(bad, r)
				}
				mu.Unlock()
			}
		}(w)
	}
	for i := 0; i < 6; i++ {
		time.Sleep(40 * time.Millisecond)
		e.setEnforcement(i%2 == 0)
	}
	time.Sleep(40 * time.Millisecond)
	close(stop)
	wg.Wait()

	if len(bad) > 0 {
		t.Fatalf("%d unexpected results while toggling, first %+v (all %v)", len(bad), bad[0], counts)
	}
	if counts["403 quota_exceeded"] == 0 {
		t.Fatalf("no quota_exceeded while the flag was on: %v (the toggle had no effect)", counts)
	}
	e.requireUsageMatchesData(e.tokenA, 1)
}

// Spec section 9: the usage gauges (the only metrics with a tenant_id label),
// the rejection counter and the flag gauge.
func TestQuotaMetrics(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(1), lim(100))
	e.put(e.tokenA, "k1", `{"value":"abc"}`) // 5 bytes
	e.put(e.tokenB, "k1", `{"value":1}`)
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k2", e.tokenA, `{"value":1}`, nil))
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k3", e.tokenA, `{"value":1}`, nil))
	e.setQuota(1, nil, lim(5))
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k1", e.tokenA, `{"value":"abcd"}`, nil))

	_, sc := e.scrape()
	want := map[string]float64{
		`storecraft_tenant_usage_keys{tenant_id="1"}`:          1,
		`storecraft_tenant_usage_bytes{tenant_id="1"}`:         5,
		`storecraft_tenant_usage_keys{tenant_id="2"}`:          1,
		`storecraft_tenant_usage_bytes{tenant_id="2"}`:         1,
		`storecraft_quota_rejections_total{dimension="keys"}`:  2,
		`storecraft_quota_rejections_total{dimension="bytes"}`: 1,
		`storecraft_quota_enforcement_enabled`:                 1,
	}
	for series, v := range want {
		name, labels := parseSeries(series)
		got, ok := sc.Value(name, labels)
		if !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", series, got, ok, v)
		}
	}
	e.setEnforcement(false)
	if _, sc = e.scrape(); sc.MustValue("storecraft_quota_enforcement_enabled", nil) != 0 {
		t.Error("enforcement gauge did not follow the flag")
	}
}

// parseSeries splits `name{a="x"}` into its name and labels.
func parseSeries(s string) (string, map[string]string) {
	name, rest, found := strings.Cut(s, "{")
	if !found {
		return s, nil
	}
	labels := map[string]string{}
	for _, pair := range strings.Split(strings.TrimSuffix(rest, "}"), ",") {
		k, v, _ := strings.Cut(pair, "=")
		labels[k] = strings.Trim(v, `"`)
	}
	return name, labels
}
