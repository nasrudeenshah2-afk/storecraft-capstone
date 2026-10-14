package replica_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"storecraft/internal/api"
	"storecraft/internal/auth"
	"storecraft/internal/replica"
	"storecraft/internal/store"
)

const (
	primaryAdmin = "primary-admin-token" // also the replication token
	replicaAdmin = "replica-admin-token" // protects the replica's own /metrics and status
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---- primary ----

type primaryNode struct {
	t   *testing.T
	st  *store.Store
	srv *httptest.Server
	url string

	mu     sync.Mutex
	sinces []string // the ?since= of every /v1/admin/changes request
	hook   func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
}

func startPrimary(t *testing.T) *primaryNode {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "primary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &primaryNode{t: t, st: st}
	h := api.New(st, api.Options{AdminToken: primaryAdmin, Log: quiet, PollTimeout: 300 * time.Millisecond, SSEHeartbeat: 100 * time.Millisecond})
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/admin/changes" {
			p.mu.Lock()
			p.sinces = append(p.sinces, r.URL.Query().Get("since"))
			hook := p.hook
			p.mu.Unlock()
			if hook != nil && hook(w, r, h) {
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	p.url = p.srv.URL
	return p
}

func (p *primaryNode) setHook(h func(w http.ResponseWriter, r *http.Request, next http.Handler) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hook = h
}

func (p *primaryNode) sinceLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sinces...)
}

// addTenant creates a tenant directly on the primary (there is no tenant API) and returns its token.
func (p *primaryNode) addTenant(name string) string {
	p.t.Helper()
	tok, err := auth.NewToken()
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.st.CreateTenant(context.Background(), name, auth.HashToken(tok)); err != nil {
		p.t.Fatal(err)
	}
	return tok
}

func (p *primaryNode) latest() int64 {
	n, err := p.st.LatestSeq(context.Background())
	if err != nil {
		p.t.Fatal(err)
	}
	return n
}

// ---- replica ----

type replicaNode struct {
	t      *testing.T
	st     *store.Store
	rep    *replica.Replica
	srv    *httptest.Server
	url    string
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startReplica(t *testing.T, primaryURL, dbPath string, tweak ...func(*replica.Config)) *replicaNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, dbPath, store.WithoutForeignKeys())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := st.EnsureReplica(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cfg := replica.Config{
		PrimaryURL: primaryURL, Token: primaryAdmin, Store: st, Log: quiet,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	rep, err := replica.New(cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	n := &replicaNode{t: t, st: st, rep: rep, cancel: cancel, done: make(chan error, 1)}
	n.srv = httptest.NewServer(api.New(st, api.Options{
		AdminToken: replicaAdmin, Role: api.RoleReplica, Replica: rep, Log: quiet,
		PollTimeout: 300 * time.Millisecond, SSEHeartbeat: 100 * time.Millisecond,
	}))
	n.url = n.srv.URL
	go func() { n.done <- rep.Run(ctx) }()
	t.Cleanup(n.stop)
	return n
}

// stop cancels replication, waits for it to finish and closes everything.
func (n *replicaNode) stop() {
	n.once.Do(func() {
		n.cancel()
		select {
		case err := <-n.done:
			if err != nil {
				n.t.Errorf("replica Run returned %v on shutdown, want nil", err)
			}
		case <-time.After(10 * time.Second):
			n.t.Error("replica did not stop within 10s of cancel")
		}
		n.srv.Close()
		n.st.Close()
	})
}

type status struct {
	Role       string `json:"role"`
	AppliedSeq int64  `json:"applied_seq"`
	PrimarySeq int64  `json:"primary_seq"`
	Lag        int64  `json:"lag"`
}

func (n *replicaNode) status() status {
	n.t.Helper()
	r := call(n.t, "GET", n.url+"/v1/replication/status", replicaAdmin, "", nil)
	if r.status != 200 {
		n.t.Fatalf("replica status: %d %s", r.status, r.body)
	}
	var s status
	if err := json.Unmarshal([]byte(r.body), &s); err != nil {
		n.t.Fatal(err)
	}
	return s
}

// waitSynced blocks until the replica has applied everything the primary has logged.
func waitSynced(t *testing.T, p *primaryNode, r *replicaNode) {
	t.Helper()
	target := p.latest()
	waitFor(t, 15*time.Second, fmt.Sprintf("replica to apply seq %d", target), func() bool {
		return r.status().AppliedSeq >= target
	})
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- HTTP helpers ----

type resp struct {
	status int
	header http.Header
	body   string
}

func call(t *testing.T, method, url, token, body string, hdr map[string]string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

func mustStatus(t *testing.T, r resp, want int) resp {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d (body %s)", r.status, want, r.body)
	}
	return r
}

func errorCode(t *testing.T, r resp) string {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error Content-Type = %q", ct)
	}
	var e struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal([]byte(r.body), &e); err != nil || e.Error.Code == "" {
		t.Fatalf("bad error envelope %q: %v", r.body, err)
	}
	return e.Error.Code
}

func put(t *testing.T, base, token, key, value string) {
	t.Helper()
	r := call(t, "PUT", base+"/v1/keys/"+key, token, `{"value":`+value+`}`, nil)
	if r.status != 200 && r.status != 201 {
		t.Fatalf("PUT %s = %d %s", key, r.status, r.body)
	}
}

func del(t *testing.T, base, token, key string) {
	t.Helper()
	mustStatus(t, call(t, "DELETE", base+"/v1/keys/"+key, token, "", nil), 204)
}

type entry struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version"`
}

func get(t *testing.T, base, token, key string) (entry, resp) {
	t.Helper()
	r := call(t, "GET", base+"/v1/keys/"+key, token, "", nil)
	var e entry
	if r.status == 200 {
		if err := json.Unmarshal([]byte(r.body), &e); err != nil {
			t.Fatal(err)
		}
	}
	return e, r
}

type changeTuple struct {
	Key     string
	Op      string
	Version int64
}

// keyChanges returns a tenant's (key, op, version) change history from /v1/changes.
func keyChanges(t *testing.T, base, token string) []changeTuple {
	t.Helper()
	r := mustStatus(t, call(t, "GET", base+"/v1/changes?since=0&limit=500", token, "", nil), 200)
	var out struct {
		Changes []changeTuple `json:"changes"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Changes
}

// ---- tests ----

// (a) put, overwrite and delete on the primary appear on the replica, with the primary's versions.
func TestReplicaFollowsPutOverwriteAndDelete(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))

	put(t, p.url, tok, "k1", `"v1"`)
	put(t, p.url, tok, "k1", `"v2"`) // overwrite
	put(t, p.url, tok, "k1", `"v3"`)
	put(t, p.url, tok, "k2", `{"n":1}`)
	del(t, p.url, tok, "k2") // delete
	put(t, p.url, tok, "k3", `[1,2,3]`)
	put(t, p.url, tok, "nested/key", `null`) // a JSON null is a real value
	waitSynced(t, p, r)

	e, res := get(t, r.url, tok, "k1")
	if res.status != 200 || string(e.Value) != `"v3"` || e.Version != 3 || res.header.Get("ETag") != `"3"` {
		t.Fatalf("replica k1 = %+v (%d, ETag %q); want v3 at version 3", e, res.status, res.header.Get("ETag"))
	}
	if _, res := get(t, r.url, tok, "k2"); res.status != 404 {
		t.Fatalf("deleted key k2 on replica: status %d, want 404", res.status)
	}
	if e, _ := get(t, r.url, tok, "nested/key"); string(e.Value) != "null" || e.Version != 1 {
		t.Fatalf("nested/key = %+v", e)
	}
	// Whole-tenant reads are identical on both nodes (keys, values, versions).
	pl := keysOf(t, mustStatus(t, call(t, "GET", p.url+"/v1/keys", tok, "", nil), 200).body)
	rl := keysOf(t, mustStatus(t, call(t, "GET", r.url+"/v1/keys", tok, "", nil), 200).body)
	if pl != rl {
		t.Fatalf("key lists differ\nprimary: %s\nreplica: %s", pl, rl)
	}
	// The replica's own change feed tells the same story (its seq numbers are its own).
	if pc, rc := keyChanges(t, p.url, tok), keyChanges(t, r.url, tok); fmt.Sprint(pc) != fmt.Sprint(rc) || len(rc) != 7 {
		t.Fatalf("change histories differ\nprimary: %v\nreplica: %v", pc, rc)
	}
	if s := r.status(); s.Role != "replica" || s.AppliedSeq != p.latest() || s.PrimarySeq != p.latest() || s.Lag != 0 {
		t.Fatalf("replica status = %+v, primary latest %d", s, p.latest())
	}
}

// (b) A tenant created AFTER the replica started can read on the replica with its token.
func TestTenantCreatedAfterReplicaStartedCanRead(t *testing.T) {
	p := startPrimary(t)
	early := p.addTenant("early")
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))
	put(t, p.url, early, "k", `"early"`)
	waitSynced(t, p, r)

	// A token the replica has never heard of is unauthorized.
	lateTok, _ := auth.NewToken()
	if got := call(t, "GET", r.url+"/v1/keys/k", lateTok, "", nil); got.status != 401 {
		t.Fatalf("unknown token on replica: %d, want 401", got.status)
	}

	// Create the tenant on the (running) primary, write, and read on the replica with the same token.
	if _, err := p.st.CreateTenant(context.Background(), "late", auth.HashToken(lateTok)); err != nil {
		t.Fatal(err)
	}
	put(t, p.url, lateTok, "hello", `"from the late tenant"`)
	waitSynced(t, p, r)

	e, res := get(t, r.url, lateTok, "hello")
	if res.status != 200 || string(e.Value) != `"from the late tenant"` || e.Version != 1 {
		t.Fatalf("late tenant read on replica = %+v (%d)", e, res.status)
	}
	// It cannot see the early tenant's data, and vice versa.
	if _, res := get(t, r.url, lateTok, "k"); res.status != 404 {
		t.Fatalf("late tenant read early tenant's key: %d", res.status)
	}
	if _, res := get(t, r.url, early, "hello"); res.status != 404 {
		t.Fatalf("early tenant read late tenant's key: %d", res.status)
	}
	// Same tenant id on both nodes.
	pt, _ := p.st.TenantByTokenHash(context.Background(), auth.HashToken(lateTok))
	rt, err := r.st.TenantByTokenHash(context.Background(), auth.HashToken(lateTok))
	if err != nil || rt.ID != pt.ID || rt.Name != "late" {
		t.Fatalf("replica tenant = %+v, %v; primary tenant = %+v", rt, err, pt)
	}
}

// (c) Stop and restart: resume from applied_seq, no duplicates, no gaps.
func TestReplicaResumesFromAppliedSeqAfterRestart(t *testing.T) {
	p := startPrimary(t)
	t1 := p.addTenant("one")
	dbPath := filepath.Join(t.TempDir(), "replica.db")

	r1 := startReplica(t, p.url, dbPath)
	for i := 1; i <= 5; i++ {
		put(t, p.url, t1, fmt.Sprintf("k%d", i), fmt.Sprintf(`"v%d"`, i))
	}
	put(t, p.url, t1, "k1", `"v1b"`)
	waitSynced(t, p, r1)
	appliedBefore := r1.status().AppliedSeq
	if appliedBefore != p.latest() {
		t.Fatalf("applied %d, primary latest %d", appliedBefore, p.latest())
	}
	r1.stop()
	time.Sleep(100 * time.Millisecond) // let any in-flight request reach the primary
	requestsBeforeRestart := len(p.sinceLog())

	// While the replica is down: more writes, a brand-new tenant, a delete.
	put(t, p.url, t1, "k6", `"v6"`)
	del(t, p.url, t1, "k2")
	t2 := p.addTenant("two")
	put(t, p.url, t2, "other", `"x"`)
	put(t, p.url, t1, "k1", `"v1c"`)

	r2 := startReplica(t, p.url, dbPath)
	waitSynced(t, p, r2)

	// It resumed from applied_seq: its first request asked for exactly that.
	after := p.sinceLog()[requestsBeforeRestart:]
	if len(after) == 0 || after[0] != fmt.Sprint(appliedBefore) {
		t.Fatalf("first request after restart had since=%v, want since=%d (log %v)", after, appliedBefore, p.sinceLog())
	}
	for _, s := range after {
		if s == "0" {
			t.Fatalf("restarted replica re-pulled from the beginning: %v", after)
		}
	}

	// Data matches the primary for both tenants.
	for name, tok := range map[string]string{"one": t1, "two": t2} {
		pl := keysOf(t, mustStatus(t, call(t, "GET", p.url+"/v1/keys", tok, "", nil), 200).body)
		rl := keysOf(t, mustStatus(t, call(t, "GET", r2.url+"/v1/keys", tok, "", nil), 200).body)
		if pl != rl {
			t.Fatalf("tenant %s differs after restart\nprimary: %s\nreplica: %s", name, pl, rl)
		}
	}
	if s := r2.status(); s.AppliedSeq != p.latest() || s.Lag != 0 {
		t.Fatalf("status after restart = %+v, primary latest %d", s, p.latest())
	}

	// No duplicates and no gaps: the replica's own log has exactly one row per
	// primary change (same tenant, kind, key, op, version, in the same order).
	primaryLog, err := p.st.AdminChanges(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	replicaLog, err := r2.st.AdminChanges(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(primaryLog) != len(replicaLog) {
		t.Fatalf("primary has %d changes, replica logged %d (duplicates or gaps)", len(primaryLog), len(replicaLog))
	}
	for i := range primaryLog {
		pc, rc := primaryLog[i], replicaLog[i]
		if pc.TenantID != rc.TenantID || pc.Kind != rc.Kind || pc.Key != rc.Key || pc.Op != rc.Op || pc.Version != rc.Version {
			t.Fatalf("change %d differs: primary %+v, replica %+v", i, pc, rc)
		}
		if rc.Seq != int64(i+1) {
			t.Fatalf("replica local seq %d at position %d", rc.Seq, i)
		}
	}
}

// (d) Writes to the replica are rejected with 409 read_only_replica; reads and /metrics work.
func TestReplicaIsReadOnly(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	put(t, p.url, tok, "existing", `"v"`)
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))
	waitSynced(t, p, r)
	localBefore, _ := r.st.LatestSeq(context.Background())

	writes := []struct {
		name, method, path, body string
		hdr                      map[string]string
	}{
		{"PUT new key", "PUT", "/v1/keys/new", `{"value":1}`, nil},
		{"PUT existing key", "PUT", "/v1/keys/existing", `{"value":"changed"}`, nil},
		{"PUT If-Match", "PUT", "/v1/keys/existing", `{"value":2}`, map[string]string{"If-Match": "1"}},
		{"PUT If-None-Match", "PUT", "/v1/keys/new2", `{"value":2}`, map[string]string{"If-None-Match": "*"}},
		{"PUT Idempotency-Key", "PUT", "/v1/keys/new3", `{"value":3}`, map[string]string{"Idempotency-Key": "abc"}},
		{"PUT invalid body", "PUT", "/v1/keys/new4", `{bad`, nil},
		{"DELETE existing", "DELETE", "/v1/keys/existing", "", nil},
		{"DELETE missing", "DELETE", "/v1/keys/nope", "", nil},
		{"DELETE Idempotency-Key", "DELETE", "/v1/keys/existing", "", map[string]string{"Idempotency-Key": "d1"}},
	}
	for _, w := range writes {
		res := call(t, w.method, r.url+w.path, tok, w.body, w.hdr)
		if res.status != 409 {
			t.Fatalf("%s: status %d, want 409 (%s)", w.name, res.status, res.body)
		}
		if code := errorCode(t, res); code != "read_only_replica" {
			t.Fatalf("%s: code %q, want read_only_replica", w.name, code)
		}
	}
	// Authentication still comes first.
	if res := call(t, "PUT", r.url+"/v1/keys/x", "", `{"value":1}`, nil); res.status != 401 {
		t.Fatalf("unauthenticated PUT on replica: %d, want 401", res.status)
	}
	if res := call(t, "PUT", r.url+"/v1/keys/x", "bogus", `{"value":1}`, nil); res.status != 401 {
		t.Fatalf("PUT with a bad token on replica: %d, want 401", res.status)
	}

	// Nothing changed - on the replica or on the primary.
	if e, res := get(t, r.url, tok, "existing"); res.status != 200 || string(e.Value) != `"v"` || e.Version != 1 {
		t.Fatalf("replica data changed: %+v (%d)", e, res.status)
	}
	for _, k := range []string{"new", "new2", "new3", "new4"} {
		if _, res := get(t, r.url, tok, k); res.status != 404 {
			t.Fatalf("key %s exists on replica after a rejected write", k)
		}
		if _, res := get(t, p.url, tok, k); res.status != 404 {
			t.Fatalf("key %s exists on primary after a rejected replica write", k)
		}
	}
	if after, _ := r.st.LatestSeq(context.Background()); after != localBefore {
		t.Fatalf("rejected writes touched the replica's log: %d -> %d", localBefore, after)
	}

	// Reads and /metrics work on the replica.
	mustStatus(t, call(t, "GET", r.url+"/v1/keys", tok, "", nil), 200)
	m := mustStatus(t, call(t, "GET", r.url+"/metrics", replicaAdmin, "", nil), 200)
	if !strings.Contains(m.body, `storecraft_http_requests_total{method="PUT",route="/v1/keys/{key}",status="409"}`) {
		t.Fatalf("replica /metrics does not show the rejected writes:\n%s", m.body)
	}
	if got := call(t, "GET", r.url+"/metrics", tok, "", nil); got.status != 401 {
		t.Fatalf("/metrics with a tenant token: %d, want 401", got.status)
	}
}

// (e) Tenant isolation holds on the replica: keys, lists, change feeds and streams.
func TestTenantIsolationOnReplica(t *testing.T) {
	p := startPrimary(t)
	a, b := p.addTenant("alpha"), p.addTenant("beta")
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))

	put(t, p.url, a, "shared", `"alpha's"`)
	put(t, p.url, b, "shared", `"beta's"`)
	put(t, p.url, a, "only-alpha", `1`)
	put(t, p.url, b, "only-beta", `2`)
	waitSynced(t, p, r)

	if e, _ := get(t, r.url, a, "shared"); string(e.Value) != `"alpha's"` {
		t.Fatalf("alpha sees %s", e.Value)
	}
	if e, _ := get(t, r.url, b, "shared"); string(e.Value) != `"beta's"` {
		t.Fatalf("beta sees %s", e.Value)
	}
	if _, res := get(t, r.url, a, "only-beta"); res.status != 404 {
		t.Fatalf("alpha read beta's key: %d", res.status)
	}
	if _, res := get(t, r.url, b, "only-alpha"); res.status != 404 {
		t.Fatalf("beta read alpha's key: %d", res.status)
	}
	for tok, bad := range map[string]string{a: "only-beta", b: "only-alpha"} {
		if l := mustStatus(t, call(t, "GET", r.url+"/v1/keys", tok, "", nil), 200).body; strings.Contains(l, bad) {
			t.Fatalf("list leaked %s: %s", bad, l)
		}
	}
	// The replica's change feeds are per tenant too, with no tenant records.
	for _, c := range []struct{ tok, bad string }{{a, "beta"}, {b, "alpha"}} {
		body := mustStatus(t, call(t, "GET", r.url+"/v1/changes?since=0&limit=500", c.tok, "", nil), 200).body
		for _, leaked := range []string{c.bad, "token_hash", "tenant_id", `"kind"`} {
			if strings.Contains(body, leaked) {
				t.Fatalf("replica /v1/changes leaked %q: %s", leaked, body)
			}
		}
	}
	// Live SSE on the replica: replicated writes reach only their own tenant's stream.
	sa, sb := openSSE(t, r.url, a), openSSE(t, r.url, b)
	sa.drain(t, 2) // the backlog from before: shared, only-alpha
	sb.drain(t, 2)
	put(t, p.url, b, "live-beta", `3`)
	if ev := sb.next(t, 10*time.Second); !strings.Contains(ev, `"key":"live-beta"`) {
		t.Fatalf("beta's replica stream got %s", ev)
	}
	sa.expectNone(t, 400*time.Millisecond)
	put(t, p.url, a, "live-alpha", `4`)
	if ev := sa.next(t, 10*time.Second); !strings.Contains(ev, `"key":"live-alpha"`) {
		t.Fatalf("alpha's replica stream got %s", ev)
	}
	sb.expectNone(t, 400*time.Millisecond)
}

// (f) A tenant token cannot use /v1/admin/changes - on the primary or on a replica.
func TestTenantTokenCannotUseAdminChanges(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	put(t, p.url, tok, "k", `"v"`)
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))
	waitSynced(t, p, r)

	for name, base := range map[string]string{"primary": p.url, "replica": r.url} {
		for _, path := range []string{"/v1/admin/changes", "/v1/admin/changes?since=0", "/v1/replication/status"} {
			res := call(t, "GET", base+path, tok, "", nil)
			if res.status != 403 || errorCode(t, res) != "forbidden" {
				t.Fatalf("%s %s with a tenant token: %d %s, want 403 forbidden", name, path, res.status, res.body)
			}
			if strings.Contains(res.body, "token_hash") {
				t.Fatalf("%s %s leaked a token hash", name, path)
			}
			if res := call(t, "GET", base+path, "", "", nil); res.status != 401 {
				t.Fatalf("%s %s without a token: %d, want 401", name, path, res.status)
			}
		}
	}
	// Each node's admin token works only on that node.
	mustStatus(t, call(t, "GET", p.url+"/v1/admin/changes?since=0", primaryAdmin, "", nil), 200)
	mustStatus(t, call(t, "GET", r.url+"/v1/admin/changes?since=0", replicaAdmin, "", nil), 200)
	if res := call(t, "GET", r.url+"/v1/admin/changes?since=0", primaryAdmin, "", nil); res.status != 401 {
		t.Fatalf("primary's admin token on the replica: %d, want 401", res.status)
	}
	// A wrong replication token is rejected by the primary; the replica reports it and applies nothing.
	bad := startReplica(t, p.url, filepath.Join(t.TempDir(), "bad.db"), func(c *replica.Config) { c.Token = "wrong-token" })
	waitFor(t, 5*time.Second, "the replica to report the auth failure", func() bool {
		return strings.Contains(bad.rep.LastError(), "401")
	})
	if n, _ := bad.st.AppliedSeq(context.Background()); n != 0 {
		t.Fatalf("a replica with a wrong token applied %d changes", n)
	}
}

// The replica retries with backoff while the primary is unavailable (errors and dropped connections) and then catches up.
func TestReplicaRetriesUntilPrimaryRecovers(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	put(t, p.url, tok, "k", `"v"`)

	var mode atomic.Value // "503" | "drop" | "ok"
	mode.Store("503")
	p.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		switch mode.Load() {
		case "503":
			http.Error(w, "primary is down", http.StatusServiceUnavailable)
			return true
		case "drop":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return true
		}
		return false
	})

	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))
	waitFor(t, 5*time.Second, "an error from a 503 primary", func() bool { return strings.Contains(r.rep.LastError(), "503") })
	mode.Store("drop")
	waitFor(t, 5*time.Second, "an error from a dropped connection", func() bool { return strings.Contains(r.rep.LastError(), "contact primary") })
	if n := r.status().AppliedSeq; n != 0 {
		t.Fatalf("applied %d changes while the primary was down", n)
	}
	// It keeps retrying (several attempts) rather than giving up or spinning.
	before := len(p.sinceLog())
	time.Sleep(600 * time.Millisecond)
	if tries := len(p.sinceLog()) - before; tries < 2 || tries > 40 {
		t.Fatalf("%d retries in 600ms with 20-100ms backoff; want a handful", tries)
	}

	mode.Store("ok")
	waitSynced(t, p, r)
	waitFor(t, 5*time.Second, "LastError to clear after recovery", func() bool { return r.rep.LastError() == "" })
	if e, res := get(t, r.url, tok, "k"); res.status != 200 || string(e.Value) != `"v"` {
		t.Fatalf("after recovery: %+v (%d)", e, res.status)
	}
}

// A batch with a missing change is refused as a whole; the replica never applies past a gap.
func TestReplicaRefusesGapsFromPrimary(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	for i := 1; i <= 5; i++ {
		put(t, p.url, tok, fmt.Sprintf("k%d", i), fmt.Sprint(i))
	}

	var tamper atomic.Bool
	tamper.Store(true)
	p.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if !tamper.Load() {
			return false
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var body map[string]json.RawMessage
		var changes []json.RawMessage
		if json.Unmarshal(rec.Body.Bytes(), &body) != nil || json.Unmarshal(body["changes"], &changes) != nil || len(changes) < 3 {
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return true
		}
		changes = append(changes[:1], changes[2:]...) // drop the second change
		body["changes"], _ = json.Marshal(changes)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
		return true
	})

	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"))
	waitFor(t, 5*time.Second, "a gap error", func() bool { return strings.Contains(r.rep.LastError(), "gap") })
	if n := r.status().AppliedSeq; n != 0 {
		t.Fatalf("applied_seq = %d after a tampered batch, want 0 (nothing applied)", n)
	}
	if _, res := get(t, r.url, tok, "k1"); res.status != 401 && res.status != 404 {
		t.Fatalf("data from a gapped batch is visible: %d", res.status)
	}

	tamper.Store(false)
	waitSynced(t, p, r)
	for i := 1; i <= 5; i++ {
		if e, res := get(t, r.url, tok, fmt.Sprintf("k%d", i)); res.status != 200 || string(e.Value) != fmt.Sprint(i) {
			t.Fatalf("k%d after recovery = %+v (%d)", i, e, res.status)
		}
	}
}

// Lag is the difference between the primary's newest seq (as reported) and what the replica has applied.
func TestReplicationStatusReportsLag(t *testing.T) {
	p := startPrimary(t)
	tok := p.addTenant("acme")
	for i := 1; i <= 6; i++ {
		put(t, p.url, tok, fmt.Sprintf("k%d", i), fmt.Sprint(i))
	}
	total := p.latest() // 1 tenant row + 6 key changes

	// Let the first batch through, then hold the primary back.
	var served atomic.Int32
	var hold atomic.Bool
	hold.Store(true)
	p.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if served.Add(1) == 1 || !hold.Load() {
			return false
		}
		http.Error(w, "held back by the test", http.StatusServiceUnavailable)
		return true
	})
	r := startReplica(t, p.url, filepath.Join(t.TempDir(), "replica.db"), func(c *replica.Config) { c.BatchLimit = 2 })

	waitFor(t, 10*time.Second, "the first batch to be applied", func() bool { return r.status().AppliedSeq == 2 })
	s := r.status()
	if s.Role != "replica" || s.AppliedSeq != 2 || s.PrimarySeq != total || s.Lag != total-2 {
		t.Fatalf("status while behind = %+v, want applied 2, primary %d, lag %d", s, total, total-2)
	}
	if ps := call(t, "GET", p.url+"/v1/replication/status", primaryAdmin, "", nil); !strings.Contains(ps.body, `"role":"primary"`) ||
		!strings.Contains(ps.body, fmt.Sprintf(`"applied_seq":%d`, total)) || !strings.Contains(ps.body, `"lag":0`) {
		t.Fatalf("primary status = %s", ps.body)
	}

	hold.Store(false)
	waitSynced(t, p, r)
	if s := r.status(); s.AppliedSeq != total || s.PrimarySeq != total || s.Lag != 0 {
		t.Fatalf("status after catching up = %+v", s)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for name, cfg := range map[string]replica.Config{
		"no url":        {Token: "t", Store: st},
		"bad scheme":    {PrimaryURL: "ftp://x", Token: "t", Store: st},
		"no host":       {PrimaryURL: "http://", Token: "t", Store: st},
		"no token":      {PrimaryURL: "http://x", Store: st},
		"no store":      {PrimaryURL: "http://x", Token: "t"},
		"not a URL":     {PrimaryURL: "::::", Token: "t", Store: st},
		"just a string": {PrimaryURL: "primary:8080", Token: "t", Store: st},
	} {
		if _, err := replica.New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}
	rep, err := replica.New(replica.Config{PrimaryURL: "http://localhost:1/", Token: "t", Store: st})
	if err != nil {
		t.Fatal(err)
	}
	// Run refuses (fatally) a database that is not a replica database.
	if err := rep.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not a replica") {
		t.Fatalf("Run on a primary DB = %v, want a not-a-replica error", err)
	}
}

// ---- minimal SSE client ----

type sseConn struct {
	events chan string
	cancel context.CancelFunc
}

func openSSE(t *testing.T, base, token string) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/changes/stream?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		t.Fatalf("stream status %d", res.StatusCode)
	}
	c := &sseConn{events: make(chan string, 256), cancel: cancel}
	go func() {
		defer res.Body.Close()
		defer close(c.events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		var data bytes.Buffer
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data: "):
				data.WriteString(line[6:])
			case line == "" && data.Len() > 0:
				c.events <- data.String()
				data.Reset()
			}
		}
	}()
	t.Cleanup(cancel)
	return c
}

func (c *sseConn) next(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(d):
		t.Fatalf("no event within %v", d)
	}
	return ""
}

func (c *sseConn) drain(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		c.next(t, 10*time.Second)
	}
}

func (c *sseConn) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if ok {
			t.Fatalf("unexpected event: %s", ev)
		}
	case <-time.After(d):
	}
}

// keysOf extracts the "keys" array of a list response. The response's "seq" is
// specific to each node, so primary and replica lists are compared by keys only.
func keysOf(t *testing.T, body string) string {
	t.Helper()
	var l struct {
		Keys json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil || l.Keys == nil {
		t.Fatalf("not a list response: %s", body)
	}
	return string(l.Keys)
}

// Regression (CI flake of TestVerifyConvergesAfterRestartMidStream, B10): a
// replica stopped while Run is still on its first step (reading applied_seq)
// must stop cleanly. Run used to return "applied seq: context canceled" as if
// replication had failed; a cancelled context is a normal stop and returns nil.
func TestRunStoppedBeforeItsFirstStepReturnsNil(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "replica.db"), store.WithoutForeignKeys())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnsureReplica(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := replica.New(replica.Config{PrimaryURL: "http://127.0.0.1:1", Token: "t", Store: st})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // stopped before Run even starts: the earliest possible stop
	if err := rep.Run(cancelled); err != nil {
		t.Fatalf("Run with a cancelled context returned %v, want nil (a normal stop)", err)
	}
}
