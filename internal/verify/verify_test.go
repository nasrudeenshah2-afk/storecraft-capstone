package verify_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"storecraft/internal/clustertest"
	"storecraft/internal/verify"
)

func cfg(p *clustertest.Primary, r *clustertest.Replica) verify.Config {
	return verify.Config{
		PrimaryURL: p.URL, ReplicaURL: r.URL,
		PrimaryToken: clustertest.PrimaryAdmin, ReplicaToken: clustertest.ReplicaAdmin,
		Timeout: 10 * time.Second, PollInterval: 20 * time.Millisecond,
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("statement changed nothing: %s", q)
	}
}

func idOf(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM tenants WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestExitCodeMapping(t *testing.T) {
	equal := &verify.Result{Equal: true}
	diverged := &verify.Result{Equal: false, Differences: []verify.Difference{{Kind: verify.KeyMismatch}}}
	for name, c := range map[string]struct {
		res  *verify.Result
		err  error
		want int
	}{
		"equal":                 {equal, nil, 0},
		"diverged":              {diverged, nil, 1},
		"timeout":               {nil, verify.ErrTimeout, 2},
		"timeout with a result": {diverged, verify.ErrTimeout, 2}, // an error always wins over a stale verdict
		"other error":           {nil, errors.New("boom"), 2},
		"no result, no error":   {nil, nil, 2},
	} {
		if got := verify.ExitCode(c.res, c.err); got != c.want {
			t.Errorf("%s: exit code %d, want %d", name, got, c.want)
		}
	}
}

func TestRunRejectsBadConfigWithoutContactingAnyone(t *testing.T) {
	base := verify.Config{PrimaryURL: "http://127.0.0.1:1", ReplicaURL: "http://127.0.0.1:2", PrimaryToken: "a", ReplicaToken: "b"}
	cases := map[string]func(c *verify.Config){
		"primary not a URL":    func(c *verify.Config) { c.PrimaryURL = "primary:8080" },
		"replica wrong scheme": func(c *verify.Config) { c.ReplicaURL = "ftp://x" },
		"no primary token":     func(c *verify.Config) { c.PrimaryToken = "" },
		"no replica token":     func(c *verify.Config) { c.ReplicaToken = "" },
	}
	for name, mutate := range cases {
		c := base
		mutate(&c)
		res, err := verify.Run(t.Context(), c)
		if err == nil || res != nil || errors.Is(err, verify.ErrTimeout) {
			t.Errorf("%s: Run = %v, %v; want a plain configuration error", name, res, err)
		}
	}
}

// A replica changed in every way at once is reported exactly: one entry per
// difference, sorted by tenant then key, and nothing else.
func TestMultipleDifferencesAreReportedExactly(t *testing.T) {
	p := clustertest.StartPrimary(t)
	alpha, beta, gamma := p.AddTenant("alpha"), p.AddTenant("beta"), p.AddTenant("gamma")
	for k, v := range map[string]string{"k1": `"v1"`, "k2": `"v2"`, "k3": `"v3"`, "k4": `"v4"`} {
		clustertest.Put(t, p.URL, alpha, k, v)
	}
	clustertest.Put(t, p.URL, beta, "b1", `"b-one"`)
	clustertest.Put(t, p.URL, beta, "b2", `"b-two"`)
	clustertest.Put(t, p.URL, gamma, "g1", `"untouched"`)
	_, _ = beta, gamma

	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)
	if res, err := verify.Run(t.Context(), cfg(p, r)); err != nil || !res.Equal {
		t.Fatalf("not clean before tampering: %+v, %v", res, err)
	}

	db := rawDB(t, r.DBPath)
	a, b := idOf(t, db, "alpha"), idOf(t, db, "beta")
	exec(t, db, `UPDATE kv SET value = ? WHERE tenant_id = ? AND key = 'k1'`, `"CHANGED"`, a)                      // value differs
	exec(t, db, `DELETE FROM kv WHERE tenant_id = ? AND key = 'k2'`, a)                                            // missing on replica
	exec(t, db, `UPDATE kv SET version = 9 WHERE tenant_id = ? AND key = 'k3'`, a)                                 // version only
	exec(t, db, `INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, 'zextra', '"x"', 4, 1)`, a) // extra on replica
	exec(t, db, `DELETE FROM kv WHERE tenant_id = ?`, b)                                                           // whole tenant missing...
	exec(t, db, `DELETE FROM tenants WHERE id = ?`, b)
	exec(t, db, `INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (99, 'orphan', '"o"', 1, 1)`) // tenant only on replica

	res, err := verify.Run(t.Context(), cfg(p, r))
	if err != nil {
		t.Fatal(err)
	}
	if res.Equal {
		t.Fatal("tampering went undetected")
	}
	type want struct {
		kind, tenant, key string
		pri, rep          *verify.KeyInfo
	}
	wants := []want{
		// STORE-210: the replica's triggers recounted alpha after the tampering,
		// so its usage counters differ too (sorted first: tenant-level, no key).
		{verify.UsageMismatch, "alpha", "", nil, nil},
		{verify.KeyMismatch, "alpha", "k1", &verify.KeyInfo{Version: 1, SHA256: sha(`"v1"`)}, &verify.KeyInfo{Version: 1, SHA256: sha(`"CHANGED"`)}},
		{verify.KeyMissingOnReplica, "alpha", "k2", &verify.KeyInfo{Version: 1, SHA256: sha(`"v2"`)}, nil},
		{verify.KeyMismatch, "alpha", "k3", &verify.KeyInfo{Version: 1, SHA256: sha(`"v3"`)}, &verify.KeyInfo{Version: 9, SHA256: sha(`"v3"`)}},
		{verify.KeyExtraOnReplica, "alpha", "zextra", nil, &verify.KeyInfo{Version: 4, SHA256: sha(`"x"`)}},
		{verify.TenantMissingOnReplica, "beta", "", nil, nil},
		{verify.TenantExtraOnReplica, "", "", nil, nil},
	}
	if len(res.Differences) != len(wants) {
		var b strings.Builder
		res.Format(&b)
		t.Fatalf("%d differences, want %d:\n%s", len(res.Differences), len(wants), b.String())
	}
	for i, w := range wants {
		d := res.Differences[i]
		if d.Kind != w.kind || d.Tenant != w.tenant || d.Key != w.key {
			t.Errorf("difference %d = %s tenant %q key %q, want %s %q %q", i, d.Kind, d.Tenant, d.Key, w.kind, w.tenant, w.key)
		}
		if fmt.Sprint(d.Primary) != fmt.Sprint(w.pri) || fmt.Sprint(d.Replica) != fmt.Sprint(w.rep) {
			t.Errorf("difference %d (%s %q): primary %v replica %v, want %v / %v", i, d.Kind, d.Key, d.Primary, d.Replica, w.pri, w.rep)
		}
	}
	// The untouched tenant is never mentioned, and ids are ascending.
	for i, d := range res.Differences {
		if d.Tenant == "gamma" {
			t.Errorf("untouched tenant reported: %+v", d)
		}
		if i > 0 && d.TenantID < res.Differences[i-1].TenantID {
			t.Errorf("differences not sorted by tenant id")
		}
	}
	if res.Differences[6].TenantID != 99 || res.Differences[5].TenantID != b {
		t.Errorf("tenant-level differences have the wrong ids: %+v", res.Differences[5:])
	}
	var out strings.Builder
	res.Format(&out)
	for _, must := range []string{"DIVERGED: 7 difference(s)", "usage_mismatch (primary 4 keys / 16 bytes, replica 4 keys / 20 bytes)", `key "k2": missing on replica`, `key "zextra": extra on replica`,
		"tenant beta (id", "missing on replica (primary has 2 keys)", "tenant (id 99, no tenant record): extra on replica (replica has 1 keys)"} {
		if !strings.Contains(out.String(), must) {
			t.Errorf("formatted output lacks %q:\n%s", must, out.String())
		}
	}
}

// Whitespace in a value is not a divergence. Before STORE-210 the primary kept
// the client's bytes while the replica stored compact JSON, and only the
// digest's canonical hashing made them agree. Since STORE-210 both nodes store
// the identical canonical bytes (AC-API-9), which this test now requires.
func TestWhitespaceOnlyDifferenceIsNotADivergence(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	clustertest.Put(t, p.URL, tok, "k", `{"a": 1,   "b": [1,  2]}`)
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)

	var onPrimary, onReplica string
	if err := rawDB(t, p.DBPath).QueryRow(`SELECT CAST(value AS TEXT) FROM kv`).Scan(&onPrimary); err != nil {
		t.Fatal(err)
	}
	if err := rawDB(t, r.DBPath).QueryRow(`SELECT CAST(value AS TEXT) FROM kv`).Scan(&onReplica); err != nil {
		t.Fatal(err)
	}
	if onPrimary != onReplica || onPrimary != `{"a":1,"b":[1,2]}` {
		t.Fatalf("stored bytes: primary %q, replica %q; want both canonical", onPrimary, onReplica)
	}
	res, err := verify.Run(t.Context(), cfg(p, r))
	if err != nil || !res.Equal || res.ValueBytes != int64(len(`{"a":1,"b":[1,2]}`)) {
		t.Fatalf("whitespace-only difference: %+v, %v", res, err)
	}
}

// While the primary is being written the digests keep moving; verify must never
// report that as a divergence. Every verdict reached during the writes compares
// both nodes at one common seq, and after the writes stop the final state matches.
func TestVerifyDuringContinuousWritesIsNeverAFalseDivergence(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			clustertest.Put(t, p.URL, tok, fmt.Sprintf("k%d", i%25), fmt.Sprint(i))
			if i%11 == 10 {
				clustertest.Del(t, p.URL, tok, fmt.Sprintf("k%d", i%25))
			}
			time.Sleep(3 * time.Millisecond)
		}
		close(stop)
	}()

	c := cfg(p, r)
	c.Timeout = 30 * time.Second
	verdicts := 0
	for running := true; running; {
		select {
		case <-stop:
			running = false
		default:
		}
		res, err := verify.Run(t.Context(), c)
		if err != nil {
			t.Fatalf("verify during writes: %v", err)
		}
		if !res.Equal {
			var b strings.Builder
			res.Format(&b)
			t.Fatalf("a moving primary was reported as divergence (verdict %d, seq %d):\n%s", verdicts, res.Seq, b.String())
		}
		verdicts++
	}
	wg.Wait()
	if verdicts < 3 {
		t.Fatalf("only %d verdicts reached during the writes", verdicts)
	}

	final, err := verify.Run(t.Context(), c)
	if err != nil || !final.Equal || final.Seq != p.Latest() {
		t.Fatalf("final verdict = %+v, %v (primary latest %d)", final, err, p.Latest())
	}
}

// A replica that never catches up is a timeout error that names both seqs.
func TestLaggingReplicaIsATimeoutNotADivergence(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)
	r.Pause()
	clustertest.Put(t, p.URL, tok, "k", `1`)

	c := cfg(p, r)
	c.Timeout = 500 * time.Millisecond
	res, err := verify.Run(t.Context(), c)
	if res != nil || !errors.Is(err, verify.ErrTimeout) {
		t.Fatalf("Run = %+v, %v; want a timeout error", res, err)
	}
	if !strings.Contains(err.Error(), "has not reached the primary's seq 2") {
		t.Fatalf("error does not explain the lag: %v", err)
	}
	if verify.ExitCode(res, err) != 2 {
		t.Fatal("a lagging replica must exit 2")
	}
}

// The primary moves on between the primary's digest and the replica's (here a
// tenant is created, which the replica applies before its digest is read). The
// two digests are then at different seqs and must not be compared: verify
// retries instead of reporting the new tenant as "extra on replica".
func TestPrimaryMovingBetweenDigestsIsRetriedNotReported(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	clustertest.Put(t, p.URL, tok, "k", `1`)
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)

	var moved atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fwd, _ := http.NewRequestWithContext(req.Context(), req.Method, p.URL+req.URL.RequestURI(), nil)
		fwd.Header.Set("Authorization", req.Header.Get("Authorization"))
		res, err := http.DefaultClient.Do(fwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		// After serving the first all-tenants digest, move the primary on and
		// let the replica catch up before verify reads the replica's digest.
		if req.URL.Path == "/v1/admin/digest" && req.URL.RawQuery == "" && moved.CompareAndSwap(false, true) {
			p.AddTenant("created-between-digests")
			r.WaitSynced(p)
		}
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		w.Write(body)
	}))
	defer proxy.Close()

	c := cfg(p, r)
	c.PrimaryURL = proxy.URL
	res, err := verify.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Load() {
		t.Fatal("test premise: the primary was never moved between digests")
	}
	if !res.Equal || res.Tenants != 2 || res.Seq != p.Latest() {
		var b strings.Builder
		res.Format(&b)
		t.Fatalf("verdict after the primary moved = %+v\n%s", res, b.String())
	}
}
