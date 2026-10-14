// Package clustertest starts a real primary and real replicas (httptest servers
// over temp SQLite files) for tests. It is test support, like metricstest: it
// imports "testing" and is only meant to be used from _test.go files.
package clustertest

import (
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
	"testing"
	"time"

	"storecraft/internal/api"
	"storecraft/internal/auth"
	"storecraft/internal/replica"
	"storecraft/internal/store"
)

const (
	PrimaryAdmin = "primary-admin-token" // also the replication token
	ReplicaAdmin = "replica-admin-token" // protects a replica's own admin routes
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// Hook lets a test interfere with the primary's /v1/admin/changes requests.
// It returns true if it handled the request itself.
type Hook func(w http.ResponseWriter, r *http.Request, next http.Handler) bool

// Primary is a running primary node.
type Primary struct {
	T      *testing.T
	St     *store.Store
	URL    string
	DBPath string
	srv    *httptest.Server

	mu     sync.Mutex
	sinces []string
	hook   Hook
}

// StartPrimary starts a primary on a fresh temp database.
func StartPrimary(t *testing.T) *Primary {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "primary.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &Primary{T: t, St: st, DBPath: dbPath}
	h := api.New(st, api.Options{AdminToken: PrimaryAdmin, Log: quiet, PollTimeout: 300 * time.Millisecond, SSEHeartbeat: 100 * time.Millisecond})
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
	p.URL = p.srv.URL
	return p
}

func (p *Primary) SetHook(h Hook) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hook = h
}

// SinceLog returns the ?since= of every /v1/admin/changes request so far.
func (p *Primary) SinceLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sinces...)
}

// AddTenant creates a tenant directly on the primary (there is no tenant API) and returns its token.
func (p *Primary) AddTenant(name string) string {
	p.T.Helper()
	tok, err := auth.NewToken()
	if err != nil {
		p.T.Fatal(err)
	}
	if _, err := p.St.CreateTenant(context.Background(), name, auth.HashToken(tok)); err != nil {
		p.T.Fatal(err)
	}
	return tok
}

// Latest is the primary's newest change seq.
func (p *Primary) Latest() int64 {
	n, err := p.St.LatestSeq(context.Background())
	if err != nil {
		p.T.Fatal(err)
	}
	return n
}

// Replica is a running replica node. Pause and Resume stop and restart only
// its pull loop; Stop shuts the whole node down.
type Replica struct {
	T      *testing.T
	St     *store.Store
	Rep    *replica.Replica
	URL    string
	DBPath string
	srv    *httptest.Server

	mu       sync.Mutex
	cancel   context.CancelFunc
	loopDone chan error
	stopped  sync.Once
}

// StartReplica starts a replica of primaryURL on dbPath (a fresh path, or the
// path of an earlier replica to resume it) and begins pulling.
func StartReplica(t *testing.T, primaryURL, dbPath string, tweak ...func(*replica.Config)) *Replica {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath, store.WithoutForeignKeys())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureReplica(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	cfg := replica.Config{
		PrimaryURL: primaryURL, Token: PrimaryAdmin, Store: st, Log: quiet,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	rep, err := replica.New(cfg)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	n := &Replica{T: t, St: st, Rep: rep, DBPath: dbPath}
	n.srv = httptest.NewServer(api.New(st, api.Options{
		AdminToken: ReplicaAdmin, Role: api.RoleReplica, Replica: rep, Log: quiet,
		PollTimeout: 300 * time.Millisecond, SSEHeartbeat: 100 * time.Millisecond,
	}))
	n.URL = n.srv.URL
	n.Resume()
	t.Cleanup(n.Stop)
	return n
}

// Pause stops the pull loop (waiting for it to finish). The node keeps serving
// reads. It is a no-op if the loop is already stopped.
func (n *Replica) Pause() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case err := <-n.loopDone:
		if err != nil {
			n.T.Errorf("replica Run returned %v on pause, want nil", err)
		}
	case <-time.After(10 * time.Second):
		n.T.Error("replica pull loop did not stop within 10s")
	}
	n.cancel = nil
}

// Resume starts the pull loop again from the database's applied_seq.
func (n *Replica) Resume() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.loopDone = make(chan error, 1)
	done := n.loopDone
	go func() { done <- n.Rep.Run(ctx) }()
}

// Stop stops pulling and closes the server and database.
func (n *Replica) Stop() {
	n.stopped.Do(func() {
		n.Pause()
		n.srv.Close()
		n.St.Close()
	})
}

// Status is /v1/replication/status.
type Status struct {
	Role       string `json:"role"`
	AppliedSeq int64  `json:"applied_seq"`
	PrimarySeq int64  `json:"primary_seq"`
	Lag        int64  `json:"lag"`
}

func (n *Replica) Status() Status {
	n.T.Helper()
	r := Call(n.T, "GET", n.URL+"/v1/replication/status", ReplicaAdmin, "", nil)
	if r.Status != 200 {
		n.T.Fatalf("replica status: %d %s", r.Status, r.Body)
	}
	var s Status
	if err := json.Unmarshal([]byte(r.Body), &s); err != nil {
		n.T.Fatal(err)
	}
	return s
}

// WaitSynced blocks until the replica has applied everything the primary has logged so far.
func (n *Replica) WaitSynced(p *Primary) {
	n.T.Helper()
	target := p.Latest()
	WaitFor(n.T, 15*time.Second, fmt.Sprintf("replica to apply seq %d", target), func() bool {
		return n.Status().AppliedSeq >= target
	})
}

// WaitFor polls cond until it is true or d passes.
func WaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Resp is a buffered HTTP response.
type Resp struct {
	Status int
	Header http.Header
	Body   string
}

// Call performs a real HTTP request.
func Call(t *testing.T, method, url, token, body string, hdr map[string]string) Resp {
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
	return Resp{res.StatusCode, res.Header, string(b)}
}

// Put writes a key (valueJSON is the raw JSON value) and requires a 200/201.
func Put(t *testing.T, base, token, key, valueJSON string) {
	t.Helper()
	r := Call(t, "PUT", base+"/v1/keys/"+key, token, `{"value":`+valueJSON+`}`, nil)
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("PUT %s = %d %s", key, r.Status, r.Body)
	}
}

// Del deletes a key and requires a 204.
func Del(t *testing.T, base, token, key string) {
	t.Helper()
	r := Call(t, "DELETE", base+"/v1/keys/"+key, token, "", nil)
	if r.Status != 204 {
		t.Fatalf("DELETE %s = %d %s", key, r.Status, r.Body)
	}
}
