package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/clustertest"
)

// verifyCmd runs `storecraft verify` against the two nodes exactly as an
// operator would (flags and environment), returning exit code, stdout, stderr.
func verifyCmd(t *testing.T, p *clustertest.Primary, r *clustertest.Replica, extra ...string) (int, string, string) {
	t.Helper()
	t.Setenv("STORECRAFT_ADMIN_TOKEN", clustertest.PrimaryAdmin)
	t.Setenv("STORECRAFT_REPLICA_ADMIN_TOKEN", clustertest.ReplicaAdmin) // the test nodes use different admin tokens
	args := append([]string{"-primary", p.URL, "-replica", r.URL, "-timeout", "10s"}, extra...)
	var out, errOut bytes.Buffer
	code := runVerify(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// rawExec changes a node's SQLite file behind its back (a separate connection).
func rawExec(t *testing.T, dbPath, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("raw statement changed nothing: %s", query)
	}
}

// tenantID looks a tenant up on the primary by its token.
func tenantID(t *testing.T, p *clustertest.Primary, token string) int64 {
	t.Helper()
	tn, err := p.St.TenantByTokenHash(context.Background(), auth.HashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	return tn.ID
}

// (a) After 100 concurrent writes and some deletes, verify reports zero divergence.
func TestVerifyAfter100ConcurrentWritesAndDeletes(t *testing.T) {
	p := clustertest.StartPrimary(t)
	toks := []string{p.AddTenant("t1"), p.AddTenant("t2"), p.AddTenant("t3")}
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			clustertest.Put(t, p.URL, toks[i%3], fmt.Sprintf("key-%03d", i), fmt.Sprint(i))
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < 100; i += 7 { // 15 deletes
		clustertest.Del(t, p.URL, toks[i%3], fmt.Sprintf("key-%03d", i))
	}
	// Overwrites too, so versions above 1 are compared.
	clustertest.Put(t, p.URL, toks[1], "key-001", `{"rewritten": true}`)

	// No manual wait: verify itself waits for the replica to catch up.
	code, out, errOut := verifyCmd(t, p, r)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	keys, bytesTotal := 0, 0
	for i := 0; i < 100; i++ {
		if i%7 == 0 {
			continue
		}
		keys++
		bytesTotal += len(fmt.Sprint(i))
	}
	bytesTotal += len(`{"rewritten":true}`) - len("1") // key-001 was rewritten
	want := fmt.Sprintf("OK: zero divergence (primary and replica both at seq %d: 3 tenants, %d keys, %d value bytes)\n", p.Latest(), keys, bytesTotal)
	if out != want {
		t.Fatalf("stdout = %q\nwant      %q", out, want)
	}
	if errOut != "" {
		t.Fatalf("unexpected stderr: %s", errOut)
	}
}

// (b) Pause the replica's pull loop, write on the primary, resume, and verify converges.
func TestVerifyConvergesAfterPauseAndResume(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	clustertest.Put(t, p.URL, tok, "before", `1`)
	r.WaitSynced(p)
	if code, out, errOut := verifyCmd(t, p, r); code != 0 {
		t.Fatalf("exit %d before pausing\n%s%s", code, out, errOut)
	}

	r.Pause()
	for i := 0; i < 20; i++ {
		clustertest.Put(t, p.URL, tok, fmt.Sprintf("while-paused-%d", i), fmt.Sprint(i))
	}
	clustertest.Del(t, p.URL, tok, "before")

	// A replica that is behind is a timeout, not a divergence.
	code, out, errOut := verifyCmd(t, p, r, "-timeout", "700ms")
	if code != 2 || out != "" || !strings.Contains(errOut, "timed out") || !strings.Contains(errOut, "has not reached the primary's seq") {
		t.Fatalf("paused replica: exit %d\nstdout: %q\nstderr: %q", code, out, errOut)
	}

	r.Resume()
	code, out, errOut = verifyCmd(t, p, r)
	if code != 0 || !strings.HasPrefix(out, "OK: zero divergence") || !strings.Contains(out, "20 keys") {
		t.Fatalf("after resume: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// (c) Restart the replica while writes are still arriving: it converges, with no duplicates.
func TestVerifyConvergesAfterRestartMidStream(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	dbPath := filepath.Join(t.TempDir(), "replica.db")
	r1 := clustertest.StartReplica(t, p.URL, dbPath)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a steady stream of writes, overwrites and deletes
		defer wg.Done()
		for i := 0; i < 200; i++ {
			key := fmt.Sprintf("k%d", i%40)
			clustertest.Put(t, p.URL, tok, key, fmt.Sprint(i))
			if i%9 == 8 {
				clustertest.Del(t, p.URL, tok, key) // just written, so it exists
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()

	time.Sleep(150 * time.Millisecond)
	r1.Stop() // the stream keeps going while the replica is down
	time.Sleep(150 * time.Millisecond)
	r2 := clustertest.StartReplica(t, p.URL, dbPath)
	wg.Wait()

	code, out, errOut := verifyCmd(t, p, r2)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	// No duplicates and no gaps: one local log row per primary change, in order.
	pl, err := p.St.AdminChanges(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := r2.St.AdminChanges(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl) != len(rl) {
		t.Fatalf("primary logged %d changes, replica %d (duplicates or gaps)", len(pl), len(rl))
	}
	for i := range pl {
		if pl[i].TenantID != rl[i].TenantID || pl[i].Kind != rl[i].Kind || pl[i].Key != rl[i].Key || pl[i].Op != rl[i].Op || pl[i].Version != rl[i].Version {
			t.Fatalf("change %d differs: primary %+v replica %+v", i, pl[i], rl[i])
		}
	}
}

// (d) A replica that starts empty after the primary already has data catches up and verifies.
func TestVerifyEmptyReplicaCatchesUpToExistingData(t *testing.T) {
	p := clustertest.StartPrimary(t)
	a, b := p.AddTenant("alpha"), p.AddTenant("beta")
	for i := 0; i < 60; i++ {
		clustertest.Put(t, p.URL, []string{a, b}[i%2], fmt.Sprintf("k%d", i), fmt.Sprintf(`{"n": %d}`, i))
	}
	for i := 0; i < 60; i += 5 {
		clustertest.Del(t, p.URL, []string{a, b}[i%2], fmt.Sprintf("k%d", i))
	}
	// k1 belongs to beta (odd i), so this creates a NEW key for alpha: 60 - 12 deleted + 1 = 49.
	clustertest.Put(t, p.URL, a, "k1", `"overwritten"`)

	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db")) // starts empty, after the data exists
	code, out, errOut := verifyCmd(t, p, r)
	if code != 0 || !strings.HasPrefix(out, "OK: zero divergence") || !strings.Contains(out, "2 tenants, 49 keys") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// (e) Changing one value directly in the replica's database file is reported exactly, with exit code 1.
func TestVerifyReportsExactlyOneTamperedValue(t *testing.T) {
	p := clustertest.StartPrimary(t)
	a, b := p.AddTenant("alpha"), p.AddTenant("beta")
	for _, k := range []string{"k1", "k2", "k3"} {
		clustertest.Put(t, p.URL, a, k, `"original-`+k+`"`)
		clustertest.Put(t, p.URL, b, k, `"b-`+k+`"`)
	}
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)
	if code, out, errOut := verifyCmd(t, p, r); code != 0 {
		t.Fatalf("not clean before tampering: exit %d\n%s%s", code, out, errOut)
	}

	alpha := tenantID(t, p, a)
	rawExec(t, r.DBPath, `UPDATE kv SET value = ? WHERE tenant_id = ? AND key = ?`, `"TAMPERED"`, alpha, "k2")

	code, out, errOut := verifyCmd(t, p, r)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if errOut != "" {
		t.Fatalf("a divergence is a verdict, not an error; stderr: %s", errOut)
	}
	wantLine := fmt.Sprintf("  tenant alpha (id %d) key \"k2\": primary version 1 sha256 %s / replica version 1 sha256 %s\n",
		alpha, sha(`"original-k2"`), sha(`"TAMPERED"`))
	// STORE-210: the replica's triggers recounted the shorter value, so verify
	// (which compares usage since AC-API-10) also reports alpha's counters.
	usageLine := fmt.Sprintf("  tenant alpha (id %d): usage_mismatch (primary 3 keys / 39 bytes, replica 3 keys / 36 bytes)\n", alpha)
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "DIVERGED: 2 difference(s)") || lines[1]+"\n" != usageLine || lines[2]+"\n" != wantLine || lines[3] != "" {
		t.Fatalf("output is not exactly the tampered key and its usage:\n%s\nwant lines:\n%s%s", out, usageLine, wantLine)
	}
	if strings.Contains(out, "beta") {
		t.Fatal("the untouched tenant was reported")
	}
}

// (f) A replica that cannot reach the primary exits 2 (timeout), never 1; so do other non-verdicts.
func TestVerifyExitCodeTwoWhenNoVerdictIsPossible(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	clustertest.Put(t, p.URL, tok, "k", `1`)

	// The primary refuses the replica's pulls (as if unreachable from the replica's side).
	p.SetHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
		return true
	})
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	time.Sleep(200 * time.Millisecond) // it has tried and failed by now
	code, out, errOut := verifyCmd(t, p, r, "-timeout", "800ms")
	if code != 2 {
		t.Fatalf("lagging replica: exit %d, want 2\nstdout: %q\nstderr: %q", code, out, errOut)
	}
	if out != "" || !strings.Contains(errOut, "timed out") || !strings.Contains(errOut, "applied_seq 0 has not reached the primary's seq") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}

	// An unreachable primary or replica.
	var o, e bytes.Buffer
	t.Setenv("STORECRAFT_ADMIN_TOKEN", clustertest.PrimaryAdmin)
	t.Setenv("STORECRAFT_REPLICA_ADMIN_TOKEN", clustertest.ReplicaAdmin)
	if code := runVerify([]string{"-primary", "http://127.0.0.1:1", "-replica", r.URL, "-timeout", "2s"}, &o, &e); code != 2 || !strings.Contains(e.String(), "cannot reach the primary") {
		t.Fatalf("dead primary: exit %d stderr %q", code, e.String())
	}
	e.Reset()
	if code := runVerify([]string{"-primary", p.URL, "-replica", "http://127.0.0.1:1", "-timeout", "2s"}, &o, &e); code != 2 || !strings.Contains(e.String(), "cannot reach the replica") {
		t.Fatalf("dead replica: exit %d stderr %q", code, e.String())
	}
	// Swapped roles, a wrong token and missing configuration are errors, not verdicts.
	e.Reset()
	if code := runVerify([]string{"-primary", r.URL, "-replica", p.URL, "-timeout", "2s"}, &o, &e); code != 2 {
		t.Fatalf("swapped nodes: exit %d stderr %q", code, e.String())
	}
	e.Reset()
	t.Setenv("STORECRAFT_ADMIN_TOKEN", "secret-wrong-token")
	if code := runVerify([]string{"-primary", p.URL, "-replica", r.URL, "-timeout", "2s"}, &o, &e); code != 2 ||
		!strings.Contains(e.String(), "401") || strings.Contains(e.String(), "secret-wrong-token") {
		t.Fatalf("wrong token: exit %d stderr %q (must mention 401 and never echo the token)", code, e.String())
	}
	for name, args := range map[string][]string{
		"no flags":      {},
		"no replica":    {"-primary", p.URL},
		"bad timeout":   {"-primary", p.URL, "-replica", r.URL, "-timeout", "0s"},
		"unknown flag":  {"-bogus"},
		"not a URL":     {"-primary", "primary:8080", "-replica", r.URL},
		"help":          {"-h"},
		"empty primary": {"-primary", "", "-replica", r.URL},
	} {
		e.Reset()
		if code := runVerify(args, &o, &e); code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %q)", name, code, e.String())
		}
	}
	t.Setenv("STORECRAFT_ADMIN_TOKEN", "")
	e.Reset()
	if code := runVerify([]string{"-primary", p.URL, "-replica", r.URL}, &o, &e); code != 2 || !strings.Contains(e.String(), "STORECRAFT_ADMIN_TOKEN") {
		t.Fatalf("no token: exit %d stderr %q", code, e.String())
	}
}

// (g) A tenant token cannot call /v1/admin/digest, on either node (and so cannot be used to verify).
func TestTenantTokenCannotCallDigest(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	clustertest.Put(t, p.URL, tok, "secret-key", `"secret-value"`)
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)

	for name, base := range map[string]string{"primary": p.URL, "replica": r.URL} {
		for _, path := range []string{"/v1/admin/digest", "/v1/admin/digest?tenant=1"} {
			res := clustertest.Call(t, "GET", base+path, tok, "", nil)
			if res.Status != 403 || !strings.Contains(res.Body, `"code":"forbidden"`) {
				t.Fatalf("%s %s with a tenant token: %d %s, want 403 forbidden", name, path, res.Status, res.Body)
			}
			if strings.Contains(res.Body, "checksum") || strings.Contains(res.Body, "secret-key") {
				t.Fatalf("%s %s leaked digest data to a tenant", name, path)
			}
			if res := clustertest.Call(t, "GET", base+path, "", "", nil); res.Status != 401 {
				t.Fatalf("%s %s without a token: %d, want 401", name, path, res.Status)
			}
		}
	}
	clustertest.Call(t, "GET", p.URL+"/v1/admin/digest", clustertest.PrimaryAdmin, "", nil)
	if res := clustertest.Call(t, "GET", p.URL+"/v1/admin/digest", clustertest.PrimaryAdmin, "", nil); res.Status != 200 {
		t.Fatalf("admin digest on primary: %d", res.Status)
	}
	if res := clustertest.Call(t, "GET", r.URL+"/v1/admin/digest", clustertest.ReplicaAdmin, "", nil); res.Status != 200 || !strings.Contains(res.Body, `"role":"replica"`) {
		t.Fatalf("admin digest on replica: %d %s", res.Status, res.Body)
	}
}
