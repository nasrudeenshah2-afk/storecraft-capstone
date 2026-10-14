package verify_test

// Acceptance tests for STORE-210 on a primary + replica pair (AC-API-9
// replication part, AC-API-10). Written in step B5 before the implementation.
// Real primary and replica nodes over HTTP with temp SQLite files.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/clustertest"
	"storecraft/internal/verify"
)

// mustStatus fails unless the response has the given status.
func mustStatus(t *testing.T, r clustertest.Resp, status int, what string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d (%s)", what, r.Status, status, r.Body)
	}
}

// adminSet changes a CAS-protected admin resource on the primary: it reads the
// current ETag with GET and sends PUT with If-Match.
func adminSet(t *testing.T, p *clustertest.Primary, getPath, putPath, body string) {
	t.Helper()
	g := clustertest.Call(t, "GET", p.URL+getPath, clustertest.PrimaryAdmin, "", nil)
	mustStatus(t, g, 200, "GET "+getPath)
	tag := g.Header.Get("ETag")
	if tag == "" {
		// GET /v1/flags has no ETag of its own: take the version from the body.
		var f struct {
			QuotaEnforcement struct{ Version int64 } `json:"quota_enforcement"`
		}
		_ = json.Unmarshal([]byte(g.Body), &f)
		tag = fmt.Sprintf(`"%d"`, f.QuotaEnforcement.Version)
	}
	mustStatus(t, clustertest.Call(t, "PUT", p.URL+putPath, clustertest.PrimaryAdmin, body, map[string]string{"If-Match": tag}), 200, "PUT "+putPath)
}

func setFlag(t *testing.T, p *clustertest.Primary, on bool) {
	t.Helper()
	adminSet(t, p, "/v1/flags", "/v1/admin/flags/quota_enforcement", fmt.Sprintf(`{"enabled":%t}`, on))
}

func setQuota(t *testing.T, p *clustertest.Primary, tenantID int64, body string) {
	t.Helper()
	path := fmt.Sprintf("/v1/admin/tenants/%d/quota", tenantID)
	adminSet(t, p, path, path, body)
}

// withoutSeq drops the "seq" field (each node reports its own log position).
func withoutSeq(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	delete(m, "seq")
	b, _ := json.Marshal(m)
	return string(b)
}

// AC-API-10: a replica serves usage, flags, the tenant list and quotas from its
// replicated state, equal to the primary's; it refuses quota and flag edits;
// verify (which also compares usage and quotas) reports zero divergence after
// a load run.
func TestQuota_AC10_ReplicaServesQuotaStateAndVerifies(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tokA := p.AddTenant("acme")
	tokB := p.AddTenant("globex")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))

	setFlag(t, p, true)
	setQuota(t, p, 1, `{"max_keys":30,"max_bytes":null}`)
	setQuota(t, p, 2, `{"max_keys":null,"max_bytes":400}`)

	// Load: 100 concurrent writers across both tenants, beyond both quotas.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := tokA
			if i%2 == 1 {
				tok = tokB
			}
			for j := 0; j < 3; j++ {
				clustertest.Call(t, "PUT", fmt.Sprintf("%s/v1/keys/k%03d-%d", p.URL, i, j), tok, `{"value":"0123456789"}`, nil)
			}
			if i%5 == 0 {
				clustertest.Call(t, "DELETE", fmt.Sprintf("%s/v1/keys/k%03d-0", p.URL, i), tok, "", nil)
			}
		}(i)
	}
	wg.Wait()
	r.WaitSynced(p)

	for _, tok := range []string{tokA, tokB} {
		pu := clustertest.Call(t, "GET", p.URL+"/v1/usage", tok, "", nil)
		ru := clustertest.Call(t, "GET", r.URL+"/v1/usage", tok, "", nil)
		mustStatus(t, pu, 200, "primary usage")
		mustStatus(t, ru, 200, "replica usage")
		if withoutSeq(t, pu.Body) != withoutSeq(t, ru.Body) {
			t.Fatalf("usage differs: primary %s, replica %s", pu.Body, ru.Body)
		}
	}
	pl := clustertest.Call(t, "GET", p.URL+"/v1/admin/tenants", clustertest.PrimaryAdmin, "", nil)
	rl := clustertest.Call(t, "GET", r.URL+"/v1/admin/tenants", clustertest.ReplicaAdmin, "", nil)
	mustStatus(t, rl, 200, "replica tenant list")
	if pl.Body != rl.Body {
		t.Fatalf("tenant list differs:\nprimary %s\nreplica %s", pl.Body, rl.Body)
	}
	for _, id := range []int{1, 2} {
		path := fmt.Sprintf("/v1/admin/tenants/%d/quota", id)
		pq := clustertest.Call(t, "GET", p.URL+path, clustertest.PrimaryAdmin, "", nil)
		rq := clustertest.Call(t, "GET", r.URL+path, clustertest.ReplicaAdmin, "", nil)
		if pq.Body != rq.Body || pq.Header.Get("ETag") != rq.Header.Get("ETag") {
			t.Fatalf("quota %d differs: primary %s %s, replica %d %s %s", id, pq.Header.Get("ETag"), pq.Body, rq.Status, rq.Header.Get("ETag"), rq.Body)
		}
	}
	flagsEqual := func() bool {
		pf := clustertest.Call(t, "GET", p.URL+"/v1/flags", tokA, "", nil)
		rf := clustertest.Call(t, "GET", r.URL+"/v1/flags", tokA, "", nil)
		return rf.Status == 200 && pf.Body == rf.Body
	}
	clustertest.WaitFor(t, 5*time.Second, "the replica's flag to equal the primary's", flagsEqual)
	setFlag(t, p, false) // a flag change alone (no key change) reaches the replica too
	clustertest.WaitFor(t, 5*time.Second, "the replica to follow a flag toggle", flagsEqual)

	// Writes of any kind are refused on the replica.
	mustStatus(t, clustertest.Call(t, "PUT", r.URL+"/v1/admin/tenants/1/quota", clustertest.ReplicaAdmin,
		`{"max_keys":1,"max_bytes":1}`, map[string]string{"If-Match": `"1"`}), 409, "quota PUT on the replica")
	mustStatus(t, clustertest.Call(t, "PUT", r.URL+"/v1/admin/flags/quota_enforcement", clustertest.ReplicaAdmin,
		`{"enabled":true}`, map[string]string{"If-Match": `"2"`}), 409, "flag PUT on the replica")

	res, err := verify.Run(t.Context(), cfg(p, r))
	if err != nil || !res.Equal {
		t.Fatalf("verify after load: %+v, %v", res, err)
	}
}

// AC-API-10: verify compares usage and quotas, so a replica whose counters or
// quota differ from the primary's is reported, by tenant.
func TestQuota_AC10_VerifyReportsUsageAndQuotaDifferences(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	p.AddTenant("globex")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	setQuota(t, p, 1, `{"max_keys":10,"max_bytes":1000}`)
	clustertest.Put(t, p.URL, tok, "k", `"v"`)
	r.WaitSynced(p)
	if res, err := verify.Run(t.Context(), cfg(p, r)); err != nil || !res.Equal {
		t.Fatalf("baseline: %+v, %v", res, err)
	}

	db := rawDB(t, r.DBPath)
	exec(t, db, `UPDATE tenant_quotas SET max_keys = 99 WHERE tenant_id = 1`)
	exec(t, db, `INSERT INTO tenant_usage(tenant_id, key_count, byte_count) VALUES (2, 7, 70)
		ON CONFLICT(tenant_id) DO UPDATE SET key_count = 7, byte_count = 70`)
	res, err := verify.Run(t.Context(), cfg(p, r))
	if err != nil || res.Equal {
		t.Fatalf("tampered quota and usage not detected: %+v, %v", res, err)
	}
	kinds := map[string]int64{}
	for _, d := range res.Differences {
		kinds[d.Kind] = d.TenantID
	}
	if kinds["quota_mismatch"] != 1 || kinds["usage_mismatch"] != 2 || len(res.Differences) != 2 {
		t.Fatalf("differences = %+v, want quota_mismatch for tenant 1 and usage_mismatch for tenant 2 only", res.Differences)
	}
}

// AC-API-9 (replication part, regression for intake finding F1): values with
// extra whitespace or <, > and & are stored byte-identically on primary and
// replica, and verify reports zero divergence.
func TestQuota_AC09_CanonicalBytesReplicateIdentically(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	values := map[string]string{
		"html":    `"<b>Tom & Jerry</b>"`,
		"ws":      `{ "a" : 1,   "b" : [1,  2] }`,
		"escaped": strings.ReplaceAll(`"Xu003cXu0026Xu003e"`, "X", `\`), // JSON escapes for <, & and >
		"mixed":   `{"t": "a < b && c > d", "n": 1.50}`,
	}
	for k, v := range values {
		clustertest.Put(t, p.URL, tok, k, v)
	}
	r.WaitSynced(p)

	read := func(path string) map[string]string {
		rows, err := rawDB(t, path).Query(`SELECT key, CAST(value AS TEXT) FROM kv ORDER BY key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			m[k] = v
		}
		return m
	}
	onP, onR := read(p.DBPath), read(r.DBPath)
	for k := range values {
		if onP[k] != onR[k] {
			t.Errorf("%s: primary stores %q, replica stores %q", k, onP[k], onR[k])
		}
	}
	if onP["html"] != `"<b>Tom & Jerry</b>"` || onP["escaped"] != `"<&>"` || onP["ws"] != `{"a":1,"b":[1,2]}` {
		t.Errorf("primary does not store canonical values: %q", onP)
	}
	res, err := verify.Run(t.Context(), cfg(p, r))
	if err != nil || !res.Equal {
		t.Fatalf("verify: %+v, %v", res, err)
	}
}
