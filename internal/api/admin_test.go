package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"storecraft/internal/auth"
)

type adminChangesResp struct {
	Changes []struct {
		Seq      int64           `json:"seq"`
		TenantID int64           `json:"tenant_id"`
		Kind     string          `json:"kind"`
		Key      string          `json:"key"`
		Op       string          `json:"op"`
		Version  int64           `json:"version"`
		Value    json.RawMessage `json:"value"`
	} `json:"changes"`
	LastSeq   int64 `json:"last_seq"`
	LatestSeq int64 `json:"latest_seq"`
}

func (e *env) adminChanges(query string) adminChangesResp {
	e.t.Helper()
	r := e.do("GET", "/v1/admin/changes"+query, adminToken, "", nil)
	if r.status != 200 {
		e.t.Fatalf("admin changes status %d: %s", r.status, r.body)
	}
	var out adminChangesResp
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		e.t.Fatalf("decode %q: %v", r.body, err)
	}
	return out
}

// The admin feed returns every tenant's changes, tenant creation included, in one seq order.
func TestAdminFeedCoversAllTenantsAndTenantCreation(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "a1", `{"value":"A1"}`)
	e.put(e.tokenB, "b1", `{"value":"B1"}`)
	e.expect(e.do("DELETE", "/v1/keys/a1", e.tokenA, "", nil), 204, "")

	feed := e.adminChanges("?since=0&limit=500")
	type row struct{ kind, key, op string }
	var got []row
	for i, c := range feed.Changes {
		if i > 0 && c.Seq != feed.Changes[i-1].Seq+1 {
			t.Fatalf("admin feed not contiguous: %d after %d", c.Seq, feed.Changes[i-1].Seq)
		}
		got = append(got, row{c.Kind, c.Key, c.Op})
	}
	want := []row{
		{"tenant", "a", "put"}, {"tenant", "b", "put"}, // created by setup
		{"key", "a1", "put"}, {"key", "b1", "put"}, {"key", "a1", "delete"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admin feed = %v, want %v", got, want)
	}
	if feed.LatestSeq != feed.LastSeq || feed.LastSeq != feed.Changes[len(feed.Changes)-1].Seq {
		t.Fatalf("last_seq %d / latest_seq %d inconsistent", feed.LastSeq, feed.LatestSeq)
	}
	// A tenant record carries its id, name and token hash (the replica needs them).
	first := feed.Changes[0]
	var rec struct {
		Name      string `json:"name"`
		TokenHash string `json:"token_hash"`
	}
	if err := json.Unmarshal(first.Value, &rec); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "tenant" || first.TenantID == 0 || rec.Name != "a" || rec.TokenHash != auth.HashToken(e.tokenA) {
		t.Fatalf("tenant record = %+v / %+v", first, rec)
	}
	if feed.Changes[4].Version != 1 || string(feed.Changes[4].Value) != "null" { // the delete
		t.Fatalf("delete change = %+v", feed.Changes[4])
	}

	// since / limit.
	page := e.adminChanges(fmt.Sprintf("?since=%d&limit=2", feed.Changes[1].Seq))
	if len(page.Changes) != 2 || page.Changes[0].Seq != feed.Changes[2].Seq || page.LastSeq != feed.Changes[3].Seq {
		t.Fatalf("page = %+v", page)
	}
	if page.LatestSeq != feed.LatestSeq {
		t.Fatalf("latest_seq = %d, want %d", page.LatestSeq, feed.LatestSeq)
	}
	for _, q := range []string{"?since=abc", "?since=-1", "?limit=0", "?limit=x"} {
		e.expect(e.do("GET", "/v1/admin/changes"+q, adminToken, "", nil), 400, "invalid_request")
	}
	// Idle: empty after the poll timeout; last_seq stays at since.
	idle := e.adminChanges(fmt.Sprintf("?since=%d", feed.LatestSeq))
	if len(idle.Changes) != 0 || idle.LastSeq != feed.LatestSeq {
		t.Fatalf("idle poll = %+v", idle)
	}
}

// The admin long-poll wakes for a write by any tenant and for a new tenant.
func TestAdminFeedLongPollWakesOnAnyCommit(t *testing.T) {
	e := setupWith(t, 30*time.Second)
	since := e.adminChanges("?since=0").LatestSeq

	wait := func(label string, trigger func()) adminChangesResp {
		t.Helper()
		done := make(chan adminChangesResp, 1)
		start := time.Now()
		go func() { done <- e.adminChanges(fmt.Sprintf("?since=%d", since)) }()
		select {
		case <-done:
			t.Fatalf("%s: poll returned before anything happened", label)
		case <-time.After(300 * time.Millisecond):
		}
		trigger()
		select {
		case r := <-done:
			if time.Since(start) > 5*time.Second {
				t.Fatalf("%s: poll took %v", label, time.Since(start))
			}
			return r
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: poll not woken", label)
		}
		return adminChangesResp{}
	}

	r := wait("tenant B write", func() { e.put(e.tokenB, "k", `{"value":1}`) })
	if len(r.Changes) != 1 || r.Changes[0].Key != "k" || r.Changes[0].Kind != "key" {
		t.Fatalf("after B's write: %+v", r)
	}
	since = r.LastSeq

	r = wait("tenant creation", func() {
		if _, err := e.st.CreateTenant(context.Background(), "late", auth.HashToken("late-token")); err != nil {
			t.Error(err)
		}
	})
	if len(r.Changes) != 1 || r.Changes[0].Kind != "tenant" || r.Changes[0].Key != "late" {
		t.Fatalf("after tenant creation: %+v", r)
	}
}

// (f) A tenant token cannot use the admin feed (or the status route); nothing leaks.
func TestTenantTokenCannotUseAdminRoutes(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "k", `{"value":1}`)
	for _, path := range []string{"/v1/admin/changes", "/v1/admin/changes?since=0", "/v1/replication/status",
		"/v1/admin/digest", "/v1/admin/digest?tenant=1"} {
		for name, tok := range map[string]string{"tenant A": e.tokenA, "tenant B": e.tokenB} {
			r := e.do("GET", path, tok, "", nil)
			if r.status != 403 {
				t.Fatalf("%s %s: status %d, want 403", name, path, r.status)
			}
			e.checkEnvelope(r, "forbidden")
			if strings.Contains(r.body, "token_hash") || strings.Contains(r.body, auth.HashToken(e.tokenA)) {
				t.Fatalf("%s %s leaked data: %s", name, path, r.body)
			}
		}
		for name, tok := range map[string]string{"none": "", "wrong": "nope"} {
			r := e.do("GET", path, tok, "", nil)
			if r.status != 401 {
				t.Fatalf("%s %s: status %d, want 401", name, path, r.status)
			}
			e.checkEnvelope(r, "unauthorized")
		}
		e.expect(e.do("GET", path, adminToken, "", nil), 200, "")
	}
	// With admin access disabled nobody gets in.
	off := setupFull(t, Options{AdminToken: noAdmin}, nil)
	for _, tok := range []string{"", "x", adminToken} {
		off.expect(off.do("GET", "/v1/admin/changes", tok, "", nil), 401, "unauthorized")
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		e.expect(e.do(m, "/v1/admin/changes", adminToken, `{}`, nil), 405, "method_not_allowed")
	}
}

// Tenant-facing feeds never carry tenant records or other tenants' changes.
func TestTenantFeedsNeverShowTenantRecordsOrOtherTenants(t *testing.T) {
	e := fastHeartbeat(t)
	e.put(e.tokenA, "a-key", `{"value":"mine"}`)
	e.put(e.tokenB, "b-secret-key", `{"value":"theirs"}`)
	if _, err := e.st.CreateTenant(context.Background(), "newcomer", auth.HashToken("newcomer-token")); err != nil {
		t.Fatal(err)
	}
	e.put(e.tokenA, "a-key-2", `{"value":"mine too"}`)

	r := e.do("GET", "/v1/changes?since=0&limit=500", e.tokenA, "", nil)
	for _, bad := range []string{"token_hash", `"kind"`, "tenant_id", "b-secret-key", "theirs", "newcomer", auth.HashToken(e.tokenB)} {
		if strings.Contains(r.body, bad) {
			t.Fatalf("/v1/changes leaked %q: %s", bad, r.body)
		}
	}
	c := e.changes(e.tokenA, "?since=0&limit=500")
	if len(c.Changes) != 2 || c.Changes[0].Key != "a-key" || c.Changes[1].Key != "a-key-2" {
		t.Fatalf("A's changes = %+v", c.Changes)
	}
	if got := e.changes(e.tokenB, "?since=0&limit=500"); len(got.Changes) != 1 || got.Changes[0].Key != "b-secret-key" {
		t.Fatalf("B's changes = %+v", got.Changes)
	}

	// The SSE stream is clean too (backlog from 0 includes the tenant rows' seqs).
	s := e.stream(e.tokenA, "?since=0", nil)
	for _, want := range []string{"a-key", "a-key-2"} {
		ev := s.next(5 * time.Second)
		if ev.key(t) != want || strings.Contains(ev.Data, "token_hash") || strings.Contains(ev.Data, "tenant") {
			t.Fatalf("stream event = %+v, want key %s", ev, want)
		}
	}
	s.expectNone(300 * time.Millisecond)
}

// Creating a tenant must not wake other tenants' long-polls.
func TestTenantCreationDoesNotWakeTenantPollers(t *testing.T) {
	e := setupWith(t, 700*time.Millisecond)
	since := e.changes(e.tokenA, "").LastSeq
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		c := e.changes(e.tokenA, fmt.Sprintf("?since=%d", since))
		if len(c.Changes) != 0 {
			t.Errorf("poll returned %+v", c)
		}
		done <- time.Since(start)
	}()
	time.Sleep(150 * time.Millisecond)
	if _, err := e.st.CreateTenant(context.Background(), "someone", auth.HashToken("someone-token")); err != nil {
		t.Fatal(err)
	}
	if elapsed := <-done; elapsed < 600*time.Millisecond {
		t.Fatalf("tenant A's poll was woken early by a tenant creation (%v)", elapsed)
	}
}

func TestReplicationStatusOnPrimary(t *testing.T) {
	e := setup(t)
	status := func() (role string, applied, primary, lag int64) {
		t.Helper()
		r := e.do("GET", "/v1/replication/status", adminToken, "", nil)
		e.expect(r, 200, "")
		var s struct {
			Role       string `json:"role"`
			AppliedSeq int64  `json:"applied_seq"`
			PrimarySeq int64  `json:"primary_seq"`
			Lag        int64  `json:"lag"`
		}
		if err := json.Unmarshal([]byte(r.body), &s); err != nil {
			t.Fatal(err)
		}
		return s.Role, s.AppliedSeq, s.PrimarySeq, s.Lag
	}
	role, applied, primary, lag := status()
	if role != "primary" || applied != 2 || primary != 2 || lag != 0 { // two tenants were created by setup
		t.Fatalf("initial status = %s %d %d %d", role, applied, primary, lag)
	}
	e.put(e.tokenA, "k", `{"value":1}`)
	e.put(e.tokenB, "k", `{"value":1}`)
	latest := e.adminChanges("?since=0&limit=500").LatestSeq
	role, applied, primary, lag = status()
	if role != "primary" || applied != latest || primary != latest || lag != 0 || latest != 4 {
		t.Fatalf("status = %s %d %d %d, want primary with latest seq %d", role, applied, primary, lag, latest)
	}
}

// GET /v1/keys returns the seq of its snapshot; following the change feed from
// it gives exactly the changes made after the list.
func TestListSeqIsWhereTheChangeFeedContinues(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "before", `{"value":1}`)
	e.put(e.tokenB, "other-tenant", `{"value":1}`)

	r := e.do("GET", "/v1/keys", e.tokenA, "", nil)
	e.expect(r, 200, "")
	var list struct {
		Keys []struct{ Key string } `json:"keys"`
		Seq  int64                  `json:"seq"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if latest := e.adminChanges("?since=0&limit=500").LatestSeq; list.Seq != latest || len(list.Keys) != 1 {
		t.Fatalf("list = %+v, latest seq %d", list, latest)
	}

	e.put(e.tokenA, "after", `{"value":2}`)
	c := e.changes(e.tokenA, fmt.Sprintf("?since=%d", list.Seq))
	if len(c.Changes) != 1 || c.Changes[0].Key != "after" {
		t.Fatalf("changes after the list's seq = %+v", c.Changes)
	}
}
