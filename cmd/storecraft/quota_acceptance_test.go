package main

// Acceptance tests for STORE-210 that need real processes (step B5, written
// before the implementation):
//   - AC-API-12: the releases/v1.0.0 binary on a database migrated by this build
//   - AC-API-13: `storecraft backfill-usage` (correct, idempotent, resumable)
//   - AC-API-6:  tenants created by `storecraft seed` get the default quota
//   - plan.md Revision 2 item 4: backfill while the server writes.
// They run the program exactly as an operator would: a binary built from this
// source tree (and the archived v1.0.0 binary), real temp SQLite files.

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/api"
	"storecraft/internal/auth"
	"storecraft/internal/clustertest"
	"storecraft/internal/store"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// currentBinary builds this source tree's storecraft once per test run.
func currentBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "storecraft-cmdtest-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "storecraft-under-test.exe")
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// runBin runs a storecraft binary to completion and returns exit code and output.
func runBin(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "STORECRAFT_ADMIN_TOKEN=")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("run %s: %v", filepath.Base(bin), err)
	}
	return 0, string(out)
}

func raw(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// requireMigrated fails clearly when this build has no usage table yet.
func requireMigrated(t *testing.T, dbPath string) {
	t.Helper()
	var n int
	if err := raw(t, dbPath).QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tenant_usage'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("STORE-210: this build's migrations create no tenant_usage table (migration 6 not implemented yet)")
	}
}

// usageRows returns tenant_usage as "id:keys:bytes" strings, and the same
// computed directly from kv, for every tenant.
func usageRows(t *testing.T, dbPath string) (stored, real map[int64]string) {
	t.Helper()
	db := raw(t, dbPath)
	stored, real = map[int64]string{}, map[int64]string{}
	q := func(query string, into map[int64]string) {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, k, b int64
			if err := rows.Scan(&id, &k, &b); err != nil {
				t.Fatal(err)
			}
			if k != 0 || b != 0 {
				into[id] = fmt.Sprintf("%d keys / %d bytes", k, b)
			}
		}
	}
	q(`SELECT tenant_id, key_count, byte_count FROM tenant_usage`, stored)
	q(`SELECT tenant_id, COUNT(*), SUM(length(CAST(value AS BLOB))) FROM kv GROUP BY tenant_id`, real)
	return stored, real
}

func requireUsageEqualsData(t *testing.T, dbPath, when string) {
	t.Helper()
	stored, real := usageRows(t, dbPath)
	if fmt.Sprint(stored) != fmt.Sprint(real) {
		t.Fatalf("%s: tenant_usage %v, but the data has %v", when, stored, real)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startServeBinary starts `bin serve` on dbPath and waits until it answers.
// The process is stopped through its own handle (never by name).
func startServeBinary(t *testing.T, bin, dbPath string) (base string, stop func()) {
	t.Helper()
	addr := freeAddr(t)
	cmd := exec.Command(bin, "serve", "-addr", addr, "-db", dbPath, "-poll-timeout", "1s")
	cmd.Env = append(os.Environ(), "STORECRAFT_ADMIN_TOKEN=synthetic-admin-for-tests")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cmd.Process.Kill()
			cmd.Wait()
		})
	}
	t.Cleanup(stop)
	base = "http://" + addr
	clustertest.WaitFor(t, 15*time.Second, filepath.Base(bin)+" to listen", func() bool {
		res, err := http.Get(base + "/metrics")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusUnauthorized
	})
	return base, stop
}

// serveInProcess serves this build's API on dbPath.
func serveInProcess(t *testing.T, dbPath, adminToken string) string {
	t.Helper()
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(api.New(st, api.Options{AdminToken: adminToken, PollTimeout: 300 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// parseSeedTokens reads "demo-N: <token>" lines printed by `storecraft seed`.
func parseSeedTokens(t *testing.T, out string) map[string]string {
	t.Helper()
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		name, tok, ok := strings.Cut(sc.Text(), ": ")
		if ok && strings.HasPrefix(name, "demo-") && !strings.Contains(tok, " ") {
			m[name] = tok
		}
	}
	if len(m) != 3 {
		t.Fatalf("seed output has %d tokens, want 3:\n%s", len(m), out)
	}
	return m
}

// AC-API-12 (+ AC-API-6 for v1-created tenants): the v1.0.0 binary runs on a
// database migrated by this build; its writes keep usage correct (the
// triggers); this build then reports the right usage without a backfill.
func TestQuota_AC12_V1BinaryOnMigratedDatabase(t *testing.T) {
	v1, err := filepath.Abs(filepath.Join("..", "..", "releases", "v1.0.0", "storecraft.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		t.Skip("releases/v1.0.0 holds a Windows binary; this check runs on Windows (scripts/ci.ps1)")
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatalf("the archived v1.0.0 binary is required for AC-API-12: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "migrated.db")

	// 1. This build creates and migrates the database and writes some data.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ownTok, _ := auth.NewToken()
	own, err := st.CreateTenant(context.Background(), "created-by-v2", auth.HashToken(ownTok))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), own.ID, "before", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	requireMigrated(t, dbPath)

	// 2. The old binary seeds tenants and serves writes on the migrated file.
	code, out := runBin(t, v1, "seed", "-db", dbPath)
	if code != 0 {
		t.Fatalf("v1.0.0 seed on the migrated database: exit %d\n%s", code, out)
	}
	tokens := parseSeedTokens(t, out)
	base, stop := startServeBinary(t, v1, dbPath)
	tok := tokens["demo-1"]
	for i := 0; i < 5; i++ {
		clustertest.Put(t, base, tok, fmt.Sprintf("k%d", i), `{ "spaced" :  "value <&>" }`)
	}
	clustertest.Put(t, base, tok, "k0", `"a much longer value than before, written by v1.0.0"`)
	clustertest.Del(t, base, tok, "k1")
	clustertest.Put(t, base, ownTok, "after", `[1, 2, 3]`)
	stop()

	// 3. Usage still equals the data (kept by the triggers, not by v1 code).
	requireUsageEqualsData(t, dbPath, "after v1.0.0 wrote and deleted")

	// 4. This build serves the right usage and the default quota, no backfill.
	newBase := serveInProcess(t, dbPath, "admin-for-ac12")
	r := clustertest.Call(t, "GET", newBase+"/v1/usage", tok, "", nil)
	if r.Status != 200 {
		t.Fatalf("GET /v1/usage on the new build: %d %s", r.Status, r.Body)
	}
	var keys, bytes int64
	if err := raw(t, dbPath).QueryRow(`SELECT COUNT(*), SUM(length(CAST(value AS BLOB))) FROM kv WHERE tenant_id =
		(SELECT id FROM tenants WHERE name = 'demo-1')`).Scan(&keys, &bytes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Body, fmt.Sprintf(`"keys":{"used":%d,"limit":1000}`, keys)) ||
		!strings.Contains(r.Body, fmt.Sprintf(`"bytes":{"used":%d,"limit":1048576}`, bytes)) {
		t.Fatalf("usage %s, want %d keys / %d bytes with the default limits", r.Body, keys, bytes)
	}
}

// legacyDB creates a migrated database holding "old" data: values stored the
// way v1.0.0 stored them (not canonical), and broken usage counters.
func legacyDB(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "legacy.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := st.CreateTenant(context.Background(), fmt.Sprintf("t%d", i), fmt.Sprintf("hash-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	requireMigrated(t, dbPath)
	db := raw(t, dbPath)
	for tenant := 1; tenant <= 3; tenant++ {
		for k := 0; k < 4; k++ {
			if _, err := db.Exec(`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, ?, ?, 1, 1700000000000000000)`,
				tenant, fmt.Sprintf("k%d", k), []byte(fmt.Sprintf(strings.ReplaceAll(`{ "n" : %d ,  "html" : "Xu003c%dXu003e" }`, "X", `\`), k, tenant))); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, q := range []string{
		`UPDATE tenant_usage SET key_count = key_count + 5, byte_count = 0 WHERE tenant_id = 1`,
		`DELETE FROM tenant_usage WHERE tenant_id = 2`,
		`UPDATE tenant_usage SET byte_count = byte_count * 3 WHERE tenant_id = 3`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return dbPath
}

// dataState is the kv contents plus tenant_usage, to compare databases.
func dataState(t *testing.T, dbPath string) string {
	t.Helper()
	rows, err := raw(t, dbPath).Query(`SELECT tenant_id, key, version, CAST(value AS TEXT) FROM kv ORDER BY tenant_id, key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, ver int64
		var k, v string
		if err := rows.Scan(&id, &k, &ver, &v); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%d/%s v%d %s\n", id, k, ver, v)
	}
	stored, _ := usageRows(t, dbPath)
	fmt.Fprint(&b, stored)
	return b.String()
}

// AC-API-13: backfill-usage sets usage to COUNT/SUM, stores old values in
// canonical form, changes nothing when run twice, and resumes after a stop.
func TestQuota_AC13_BackfillIsCorrectIdempotentAndResumable(t *testing.T) {
	bin := currentBinary(t)
	dbPath := legacyDB(t, t.TempDir())

	code, out := runBin(t, bin, "backfill-usage", "-db", dbPath)
	if code != 0 {
		t.Fatalf("backfill-usage: exit %d\n%s", code, out)
	}
	requireUsageEqualsData(t, dbPath, "after backfill")
	var v string
	if err := raw(t, dbPath).QueryRow(`SELECT CAST(value AS TEXT) FROM kv WHERE tenant_id = 2 AND key = 'k3'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != `{"n":3,"html":"<2>"}` {
		t.Fatalf("value after backfill = %q, want the canonical form", v)
	}
	full := dataState(t, dbPath)

	for _, args := range [][]string{{"backfill-usage", "-db", dbPath}, {"backfill-usage", "-db", dbPath, "-restart"}} {
		if code, out := runBin(t, bin, args...); code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, code, out)
		}
		if again := dataState(t, dbPath); again != full {
			t.Fatalf("%v changed the data:\nbefore\n%s\nafter\n%s", args, full, again)
		}
	}

	// Stopped after one tenant, then run again: same final result.
	resumed := legacyDB(t, t.TempDir())
	code, out = runBin(t, bin, "backfill-usage", "-db", resumed, "-limit", "1")
	if code != 0 || !strings.Contains(out, "run again") {
		t.Fatalf("backfill-usage -limit 1: exit %d\n%s", code, out)
	}
	if stored, real := usageRows(t, resumed); stored[1] != real[1] || stored[3] == real[3] {
		t.Fatalf("after a stop at one tenant: tenant 1 must be fixed and tenant 3 not yet: stored %v, real %v", stored, real)
	}
	if code, out = runBin(t, bin, "backfill-usage", "-db", resumed); code != 0 {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	if got := dataState(t, resumed); got != full {
		t.Fatalf("resumed run differs from a full run:\nfull\n%s\nresumed\n%s", full, got)
	}
}

// AC-API-6: tenants created by `storecraft seed` have no quota record and get
// the default quota at version 0.
func TestQuota_AC06_SeededTenantsGetTheDefaultQuota(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seeded.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTenants(context.Background(), st, 3); err != nil {
		t.Fatal(err)
	}
	st.Close()
	base := serveInProcess(t, dbPath, "admin-for-ac06")
	for id := 1; id <= 3; id++ {
		r := clustertest.Call(t, "GET", fmt.Sprintf("%s/v1/admin/tenants/%d/quota", base, id), "admin-for-ac06", "", nil)
		if r.Status != 200 || r.Header.Get("ETag") != `"0"` || !strings.Contains(r.Body, `"max_keys":1000,"max_bytes":1048576,"version":0`) {
			t.Fatalf("seeded tenant %d quota: %d %s %s", id, r.Status, r.Header.Get("ETag"), r.Body)
		}
	}
}

// plan.md Revision 2 item 4: backfill running while the server writes never
// leaves usage different from the data.
func TestQuota_BackfillDuringLiveWrites(t *testing.T) {
	bin := currentBinary(t)
	dbPath := legacyDB(t, t.TempDir())
	base := serveInProcess(t, dbPath, "admin-for-live")
	tok, _ := auth.NewToken()
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTenant(context.Background(), "live", auth.HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				path := fmt.Sprintf("%s/v1/keys/w%d-%d", base, w, i%7)
				method, body := "PUT", fmt.Sprintf(`{"value":"%s"}`, strings.Repeat("x", i%13))
				if i%4 == 3 {
					method, body = "DELETE", ""
				}
				req, _ := http.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+tok)
				if res, err := http.DefaultClient.Do(req); err == nil {
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
				}
			}
		}(w)
	}
	for i := 0; i < 3; i++ {
		if code, out := runBin(t, bin, "backfill-usage", "-db", dbPath, "-restart"); code != 0 {
			close(stop)
			wg.Wait()
			t.Fatalf("backfill during writes: exit %d\n%s", code, out)
		}
	}
	close(stop)
	wg.Wait()
	requireUsageEqualsData(t, dbPath, "after backfills during live writes")
}
