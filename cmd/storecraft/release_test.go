package main

// Release readiness (B14): checks spec.md section 8's INFERENCE that an
// un-upgraded v1.0.0 replica keeps replicating from a STORE-210 primary (it
// sees quota changes as tenant rows of an existing tenant and skips them, and
// ignores the feed's flags field). This is what makes "replicas first" only a
// recommendation rather than a hard requirement.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"storecraft/internal/clustertest"
)

func TestReleaseV1ReplicaKeepsReplicatingFromNewPrimary(t *testing.T) {
	v1, _ := filepath.Abs(filepath.Join("..", "..", "releases", "v1.0.0", "storecraft.exe"))
	if runtime.GOOS != "windows" {
		t.Skip("releases/v1.0.0 holds a Windows binary; this check runs on Windows (scripts/ci.ps1)")
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatalf("the archived v1.0.0 binary is required: %v", err)
	}
	p := clustertest.StartPrimary(t) // this build
	tok := p.AddTenant("acme")
	other := p.AddTenant("globex")

	addr := freeAddr(t)
	cmd := exec.Command(v1, "serve", "-role=replica", "-primary="+p.URL, "-addr", addr, "-db", filepath.Join(t.TempDir(), "v1-replica.db"), "-poll-timeout", "1s")
	cmd.Env = append(os.Environ(), "STORECRAFT_ADMIN_TOKEN=v1-replica-admin", "STORECRAFT_REPLICATION_TOKEN="+clustertest.PrimaryAdmin)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) // our own child, by handle
	replicaURL := "http://" + addr

	// The new primary logs everything a v1 replica has never seen: quota
	// changes (tenant rows with subject "quota"), flag changes (feed field),
	// canonical values, and quota rejections in between.
	set := func(path, body string) {
		g := clustertest.Call(t, "GET", p.URL+path, clustertest.PrimaryAdmin, "", nil)
		version := g.Header.Get("ETag")
		if version == "" {
			version = `"0"`
		}
		put := path
		if path == "/v1/flags" {
			put = "/v1/admin/flags/quota_enforcement"
		}
		if r := clustertest.Call(t, "PUT", p.URL+put, clustertest.PrimaryAdmin, body, map[string]string{"If-Match": version}); r.Status != 200 {
			t.Fatalf("PUT %s: %d %s", put, r.Status, r.Body)
		}
	}
	set("/v1/flags", `{"enabled":true}`)
	set("/v1/admin/tenants/1/quota", `{"max_keys":5,"max_bytes":null}`)
	for i := 0; i < 8; i++ {
		clustertest.Call(t, "PUT", fmt.Sprintf("%s/v1/keys/k%d", p.URL, i), tok, `{"value":{"i":`+fmt.Sprint(i)+`}}`, nil) // 5 succeed, 3 rejected
	}
	set("/v1/admin/tenants/1/quota", `{"max_keys":null,"max_bytes":null}`)
	clustertest.Put(t, p.URL, other, "after-quota-changes", `"still replicating"`)
	clustertest.Del(t, p.URL, tok, "k0")

	// The v1 replica reaches the primary's newest seq (it did not stall on the
	// new kinds of rows) and serves the same keys.
	target := p.Latest()
	clustertest.WaitFor(t, 20*time.Second, fmt.Sprintf("the v1.0.0 replica to apply seq %d", target), func() bool {
		r, err := http.NewRequest("GET", replicaURL+"/v1/replication/status", nil)
		if err != nil {
			return false
		}
		r.Header.Set("Authorization", "Bearer v1-replica-admin")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		var st struct {
			AppliedSeq int64 `json:"applied_seq"`
		}
		return json.NewDecoder(res.Body).Decode(&st) == nil && st.AppliedSeq >= target
	})
	for _, c := range []struct{ token, key string }{{tok, "k1"}, {tok, "k4"}, {other, "after-quota-changes"}} {
		pr := clustertest.Call(t, "GET", p.URL+"/v1/keys/"+c.key, c.token, "", nil)
		rr := clustertest.Call(t, "GET", replicaURL+"/v1/keys/"+c.key, c.token, "", nil)
		if pr.Status != 200 || rr.Status != 200 {
			t.Fatalf("%s: primary %d, v1 replica %d %s", c.key, pr.Status, rr.Status, rr.Body)
		}
	}
	for _, missing := range []string{"k0", "k5"} { // deleted, and rejected by the quota
		if r := clustertest.Call(t, "GET", replicaURL+"/v1/keys/"+missing, tok, "", nil); r.Status != 404 {
			t.Fatalf("%s on the v1 replica: %d, want 404", missing, r.Status)
		}
	}
	if r := clustertest.Call(t, "GET", replicaURL+"/v1/usage", tok, "", nil); r.Status != 404 || !strings.Contains(r.Body, "no such route") {
		t.Fatalf("v1 replica /v1/usage: %d %s (v1.0.0 has no usage API)", r.Status, r.Body)
	}
}
