package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type changeResp struct {
	Changes []struct {
		Seq       int64           `json:"seq"`
		Key       string          `json:"key"`
		Op        string          `json:"op"`
		Version   int64           `json:"version"`
		Value     json.RawMessage `json:"value"`
		ValueSize int64           `json:"value_size"`
		TS        string          `json:"ts"`
	} `json:"changes"`
	LastSeq int64 `json:"last_seq"`
}

func parseChanges(t *testing.T, r resp) changeResp {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("changes status = %d, body %q", r.status, r.body)
	}
	var c changeResp
	if err := json.Unmarshal([]byte(r.body), &c); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return c
}

func (e *env) changes(token, query string) changeResp {
	e.t.Helper()
	return parseChanges(e.t, e.do("GET", "/v1/changes"+query, token, "", nil))
}

func (e *env) put(token, key, body string) {
	e.t.Helper()
	if r := e.do("PUT", "/v1/keys/"+key, token, body, nil); r.status != 200 && r.status != 201 {
		e.t.Fatalf("PUT %s = %d %s", key, r.status, r.body)
	}
}

// putConcurrently performs n PUTs over real HTTP at once; key(i) picks the key.
func (e *env) putConcurrently(n int, key func(i int) string) {
	e.t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("PUT", e.url+"/v1/keys/"+key(i), strings.NewReader(fmt.Sprintf(`{"value":%d}`, i)))
			req.Header.Set("Authorization", "Bearer "+e.tokenA)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses[i] = -1
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			statuses[i] = res.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	for i, s := range statuses {
		if s != 200 && s != 201 {
			e.t.Fatalf("concurrent PUT %d status = %d", i, s)
		}
	}
}

// (1) 100 concurrent PUTs -> 100 change rows, no gaps, no duplicate seq, in order per key.
func TestChangesConcurrentPuts(t *testing.T) {
	e := setup(t)
	e.putConcurrently(100, func(i int) string { return fmt.Sprintf("key-%d", i%10) })

	c := e.changes(e.tokenA, "?since=0&limit=500")
	if len(c.Changes) != 100 {
		t.Fatalf("got %d changes, want 100", len(c.Changes))
	}
	nextVersion := map[string]int64{}
	for i, ch := range c.Changes {
		if i > 0 && ch.Seq != c.Changes[i-1].Seq+1 {
			t.Fatalf("gap/duplicate/out of order: seq %d after %d", ch.Seq, c.Changes[i-1].Seq)
		}
		nextVersion[ch.Key]++
		if ch.Op != "put" || ch.Version != nextVersion[ch.Key] {
			t.Fatalf("change %d (%s) version = %d, want %d: per-key order broken", i, ch.Key, ch.Version, nextVersion[ch.Key])
		}
	}
	if len(nextVersion) != 10 {
		t.Fatalf("keys seen = %d, want 10", len(nextVersion))
	}
	if c.LastSeq != c.Changes[99].Seq {
		t.Fatalf("last_seq = %d, want %d", c.LastSeq, c.Changes[99].Seq)
	}

	// Paginating with last_seq yields the same 100 changes.
	var seen []int64
	since := int64(0)
	// Stop once all 100 are seen: asking for a page past the end would
	// (correctly) long-poll for the full default timeout.
	for len(seen) < 100 {
		p := e.changes(e.tokenA, fmt.Sprintf("?since=%d&limit=30", since))
		if len(p.Changes) == 0 {
			t.Fatalf("pagination ended early at %d changes", len(seen))
		}
		for _, ch := range p.Changes {
			seen = append(seen, ch.Seq)
		}
		since = p.LastSeq
	}
	if len(seen) != 100 {
		t.Fatalf("paginated %d changes, want 100", len(seen))
	}
	for i, s := range seen {
		if s != c.Changes[i].Seq {
			t.Fatalf("page %d seq %d != %d", i, s, c.Changes[i].Seq)
		}
	}
}

// (2) A long-poll returns as soon as a write happens.
func TestLongPollReturnsOnWrite(t *testing.T) {
	e := setupWith(t, 30*time.Second)
	e.put(e.tokenA, "before", `{"value":0}`)
	since := e.changes(e.tokenA, "").LastSeq

	type result struct {
		c       changeResp
		elapsed time.Duration
	}
	done := make(chan result, 1)
	pollStart := time.Now()
	go func() {
		c := e.changes(e.tokenA, fmt.Sprintf("?since=%d", since))
		done <- result{c, time.Since(pollStart)}
	}()

	select {
	case r := <-done:
		t.Fatalf("long-poll returned before any write: %+v", r.c)
	case <-time.After(400 * time.Millisecond):
	}

	writeAt := time.Now()
	e.put(e.tokenA, "wake", `{"value":"hello"}`)

	select {
	case r := <-done:
		if lag := time.Since(writeAt); lag > 2*time.Second {
			t.Fatalf("long-poll took %v after the write", lag)
		}
		if len(r.c.Changes) != 1 || r.c.Changes[0].Key != "wake" || r.c.Changes[0].Seq != since+1 || r.c.LastSeq != since+1 {
			t.Fatalf("poll result = %+v", r.c)
		}
		if r.elapsed >= 30*time.Second {
			t.Fatalf("poll waited for the timeout (%v)", r.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long-poll did not return after a write")
	}
}

func TestLongPollTimeoutReturnsEmpty(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	start := time.Now()
	r := e.do("GET", "/v1/changes?since=7", e.tokenA, "", nil)
	elapsed := time.Since(start)
	c := parseChanges(t, r)
	if len(c.Changes) != 0 || c.LastSeq != 7 || !strings.Contains(r.body, `"changes":[]`) {
		t.Fatalf("timeout body = %s", r.body)
	}
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("timeout after %v, want ~300ms", elapsed)
	}
	// Data already present returns immediately, not after the timeout.
	e.put(e.tokenA, "k", `{"value":1}`)
	start = time.Now()
	if c := e.changes(e.tokenA, "?since=0"); len(c.Changes) != 1 {
		t.Fatalf("changes = %+v", c)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("existing change took %v to return", time.Since(start))
	}
}

// (3) A tenant never sees another tenant's changes, including via long-poll.
func TestChangesTenantIsolation(t *testing.T) {
	e := setupWith(t, 700*time.Millisecond)
	e.put(e.tokenA, "a-secret", `{"value":"A"}`)
	e.put(e.tokenB, "b-key", `{"value":"B"}`)
	e.put(e.tokenA, "a-secret-2", `{"value":"A2"}`)

	cb := e.changes(e.tokenB, "?since=0")
	if len(cb.Changes) != 1 || cb.Changes[0].Key != "b-key" {
		t.Fatalf("B sees %+v", cb)
	}
	ca := e.changes(e.tokenA, "?since=0")
	if len(ca.Changes) != 2 {
		t.Fatalf("A sees %+v", ca)
	}
	for _, ch := range ca.Changes {
		if ch.Key == "b-key" {
			t.Fatal("A sees B's change")
		}
	}

	// B long-polls past its last change; A's write must not satisfy it.
	since := cb.LastSeq
	type result struct {
		c       changeResp
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		c := e.changes(e.tokenB, fmt.Sprintf("?since=%d", since))
		done <- result{c, time.Since(start)}
	}()
	time.Sleep(150 * time.Millisecond)
	e.put(e.tokenA, "a-late", `{"value":"late"}`)
	r := <-done
	if len(r.c.Changes) != 0 || r.c.LastSeq != since {
		t.Fatalf("B's poll saw A's write: %+v", r.c)
	}
	if r.elapsed < 600*time.Millisecond {
		t.Fatalf("B's poll was woken early by A (%v)", r.elapsed)
	}
}

func TestChangesQueryParams(t *testing.T) {
	e := setup(t)
	e.putConcurrently(120, func(i int) string { return fmt.Sprintf("k%03d", i) })

	if c := e.changes(e.tokenA, ""); len(c.Changes) != 100 {
		t.Fatalf("default limit returned %d, want 100", len(c.Changes))
	}
	if c := e.changes(e.tokenA, "?limit=500"); len(c.Changes) != 120 {
		t.Fatalf("limit=500 returned %d, want 120", len(c.Changes))
	}
	if c := e.changes(e.tokenA, "?limit=100000"); len(c.Changes) != 120 {
		t.Fatalf("over-max limit returned %d, want it clamped (all 120)", len(c.Changes))
	}
	all := e.changes(e.tokenA, "?limit=500")
	c := e.changes(e.tokenA, "?limit=2")
	if len(c.Changes) != 2 || c.LastSeq != c.Changes[1].Seq || c.Changes[0].Seq != all.Changes[0].Seq {
		t.Fatalf("limit=2 = %+v", c)
	}
	mid := all.Changes[59].Seq
	c = e.changes(e.tokenA, fmt.Sprintf("?since=%d&limit=500", mid))
	if len(c.Changes) != 60 || c.Changes[0].Seq != mid+1 {
		t.Fatalf("since filter: %d changes, first seq %d (mid %d)", len(c.Changes), c.Changes[0].Seq, mid)
	}
	for i := 1; i < len(c.Changes); i++ {
		if c.Changes[i].Seq <= c.Changes[i-1].Seq {
			t.Fatal("not ascending")
		}
	}

	for _, q := range []string{
		"?since=abc", "?since=-1", "?since=", "?since=1.5", "?since=99999999999999999999",
		"?limit=0", "?limit=-5", "?limit=x", "?limit=",
	} {
		e.expect(e.do("GET", "/v1/changes"+q, e.tokenA, "", nil), 400, "invalid_request")
	}
}

func TestChangesAuthAndMethods(t *testing.T) {
	e := setup(t)
	e.expect(e.do("GET", "/v1/changes", "", "", nil), 401, "unauthorized")
	e.expect(e.do("GET", "/v1/changes", "wrong", "", nil), 401, "unauthorized")
	e.expect(e.do("GET", "/v1/changes", adminToken, "", nil), 403, "forbidden")
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		r := e.do(m, "/v1/changes", e.tokenA, `{"value":1}`, nil)
		e.expect(r, 405, "method_not_allowed")
		if r.header.Get("Allow") != "GET" {
			t.Fatalf("%s Allow = %q", m, r.header.Get("Allow"))
		}
	}
}

func TestChangeShapesAndNoRowsOnRejectedWrites(t *testing.T) {
	e := setup(t)
	e.put(e.tokenA, "doc", `{"value":{"a":1}}`)
	e.expect(e.do("DELETE", "/v1/keys/doc", e.tokenA, "", nil), 204, "")

	// Rejected writes must add nothing.
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":1}`, map[string]string{"If-Match": "5"}), 412, "")
	e.put(e.tokenA, "doc", `{"value":2}`)
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":3}`, map[string]string{"If-None-Match": "*"}), 412, "")
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{bad`, nil), 400, "")
	e.expect(e.do("DELETE", "/v1/keys/nope", e.tokenA, "", nil), 404, "")

	c := e.changes(e.tokenA, "")
	if len(c.Changes) != 3 {
		t.Fatalf("got %d changes, want 3: %+v", len(c.Changes), c)
	}
	put, del, again := c.Changes[0], c.Changes[1], c.Changes[2]
	if put.Op != "put" || put.Key != "doc" || put.Version != 1 || string(put.Value) != `{"a":1}` || put.ValueSize != 7 {
		t.Fatalf("put change = %+v", put)
	}
	if _, err := time.Parse(time.RFC3339Nano, put.TS); err != nil {
		t.Fatalf("ts %q: %v", put.TS, err)
	}
	if del.Op != "delete" || del.Version != 1 || string(del.Value) != "null" || del.ValueSize != 0 {
		t.Fatalf("delete change = %+v", del)
	}
	if again.Op != "put" || again.Version != 1 || string(again.Value) != "2" {
		t.Fatalf("recreate change = %+v", again)
	}
}

// (4) A failing write leaves no change row (and no kv change), without leaking details.
func TestFailingWriteLeavesNoChangeRow(t *testing.T) {
	e := setup(t)
	e.put(e.tokenA, "existing", `{"value":1}`)
	before := e.changes(e.tokenA, "")

	// Inject a real failure into the change-log insert via a trigger.
	raw, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_boom BEFORE INSERT ON changes WHEN NEW.key = 'boom' BEGIN SELECT RAISE(ABORT, 'trigger-secret-xyz'); END`); err != nil {
		t.Fatal(err)
	}

	r := e.do("PUT", "/v1/keys/boom", e.tokenA, `{"value":1}`, nil)
	e.expect(r, 500, "internal")
	if strings.Contains(r.body, "trigger-secret-xyz") || strings.Contains(strings.ToLower(r.body), "sqlite") {
		t.Fatalf("internal error leaked details: %s", r.body)
	}
	e.expect(e.do("GET", "/v1/keys/boom", e.tokenA, "", nil), 404, "not_found")

	after := e.changes(e.tokenA, "")
	if len(after.Changes) != len(before.Changes) || after.LastSeq != before.LastSeq {
		t.Fatalf("change log moved after failed write: %+v -> %+v", before, after)
	}
	// The next good write follows directly (no burned seq).
	e.put(e.tokenA, "fine", `{"value":2}`)
	if c := e.changes(e.tokenA, fmt.Sprintf("?since=%d", before.LastSeq)); len(c.Changes) != 1 || c.Changes[0].Seq != before.LastSeq+1 {
		t.Fatalf("after recovery: %+v", c)
	}
}
