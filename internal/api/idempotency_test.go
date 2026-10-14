package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func idem(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }

func (e *env) changeCount() int { return len(e.changes(e.tokenA, "?limit=500&since=0").Changes) }

// (d) Replay returns the stored response and does not write again.
func TestIdempotentPutReplay(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	r1 := e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"x"}`, idem("abc"))
	e.expect(r1, 201, "")
	if r1.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first response marked as replay")
	}
	if r1.body != `{"key":"k","version":1}`+"\n" || r1.header.Get("ETag") != `"1"` {
		t.Fatalf("first response = %q etag %q", r1.body, r1.header.Get("ETag"))
	}

	for i := 0; i < 3; i++ {
		r2 := e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"x"}`, idem("abc"))
		if r2.status != r1.status || r2.body != r1.body || r2.header.Get("ETag") != r1.header.Get("ETag") ||
			r2.header.Get("Content-Type") != "application/json" {
			t.Fatalf("replay %d differs: %+v vs %+v", i, r2, r1)
		}
		if r2.header.Get("Idempotent-Replayed") != "true" {
			t.Fatal("replay not marked")
		}
	}
	g := e.do("GET", "/v1/keys/k", e.tokenA, "", nil)
	if _, ver := valueOf(t, g); ver != 1 {
		t.Fatalf("version after replays = %d, want 1", ver)
	}
	if n := e.changeCount(); n != 1 {
		t.Fatalf("change rows = %d, want 1", n)
	}
	// Without the header the same request is a normal second write.
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"x"}`, nil), 200, "")
	if n := e.changeCount(); n != 2 {
		t.Fatalf("change rows = %d, want 2", n)
	}
}

func TestIdempotentDeleteReplay(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "k", `{"value":1}`)
	e.expect(e.do("DELETE", "/v1/keys/k", e.tokenA, "", idem("del-1")), 204, "")
	e.put(e.tokenA, "k", `{"value":2}`) // key exists again

	r := e.do("DELETE", "/v1/keys/k", e.tokenA, "", idem("del-1"))
	e.expect(r, 204, "")
	if r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("delete replay not marked")
	}
	// The replay must not have deleted the re-created key, nor logged a change.
	e.expect(e.do("GET", "/v1/keys/k", e.tokenA, "", nil), 200, "")
	if n := e.changeCount(); n != 3 { // put, delete, put
		t.Fatalf("change rows = %d, want 3", n)
	}
}

// (e) Same key, different method / path / body -> 422.
func TestIdempotencyConflict(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"one"}`, idem("key1")), 201, "")
	before := e.changeCount()

	for name, c := range map[string]struct{ method, path, body string }{
		"different body":   {"PUT", "/v1/keys/k", `{"value":"two"}`},
		"different path":   {"PUT", "/v1/keys/other", `{"value":"one"}`},
		"different method": {"DELETE", "/v1/keys/k", ""},
	} {
		r := e.do(c.method, c.path, e.tokenA, c.body, idem("key1"))
		if r.status != 422 {
			t.Fatalf("%s: status %d, want 422 (%s)", name, r.status, r.body)
		}
		e.checkEnvelope(r, "idempotency_conflict")
	}
	if n := e.changeCount(); n != before {
		t.Fatalf("conflicting requests wrote: %d -> %d changes", before, n)
	}
	g := e.do("GET", "/v1/keys/k", e.tokenA, "", nil)
	if v, ver := valueOf(t, g); v != `"one"` || ver != 1 {
		t.Fatalf("value changed: %s v%d", v, ver)
	}
	e.expect(e.do("GET", "/v1/keys/other", e.tokenA, "", nil), 404, "not_found")
}

// (f) Many concurrent requests with one key produce exactly one write.
func TestIdempotentConcurrentSameKeyOneWrite(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	const n = 20
	type result struct {
		status   int
		body     string
		replayed bool
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("PUT", e.url+"/v1/keys/once", strings.NewReader(`{"value":"same"}`))
			req.Header.Set("Authorization", "Bearer "+e.tokenA)
			req.Header.Set("Idempotency-Key", "shared-key")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i].status = -1
				return
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			results[i] = result{res.StatusCode, string(b), res.Header.Get("Idempotent-Replayed") == "true"}
		}(i)
	}
	close(start)
	wg.Wait()

	replays := 0
	for i, r := range results {
		if r.status != 201 || r.body != results[0].body {
			t.Fatalf("response %d = %+v, want identical 201 responses", i, r)
		}
		if r.replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("%d replays, want %d (exactly one real execution)", replays, n-1)
	}
	g := e.do("GET", "/v1/keys/once", e.tokenA, "", nil)
	if _, ver := valueOf(t, g); ver != 1 {
		t.Fatalf("version = %d, want 1 (20 writes would give 20)", ver)
	}
	if c := e.changeCount(); c != 1 {
		t.Fatalf("change rows = %d, want exactly 1", c)
	}
}

// Racing different bodies under one key: one executes, the others conflict.
func TestIdempotentConcurrentDifferentBodies(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	const n = 20
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"value":"body-%d"}`, i%2)
			req, _ := http.NewRequest("PUT", e.url+"/v1/keys/race", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+e.tokenA)
			req.Header.Set("Idempotency-Key", "race-key")
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

	g := e.do("GET", "/v1/keys/race", e.tokenA, "", nil)
	won, ver := valueOf(t, g)
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}
	for i, s := range statuses {
		want := 422
		if won == fmt.Sprintf(`"body-%d"`, i%2) {
			want = 201
		}
		if s != want {
			t.Fatalf("request %d (body-%d, winner %s): status %d, want %d", i, i%2, won, s, want)
		}
	}
	if c := e.changeCount(); c != 1 {
		t.Fatalf("change rows = %d, want 1", c)
	}
}

func TestIdempotencyKeysAreScopedPerTenant(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	ra := e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"A"}`, idem("same-key"))
	rb := e.do("PUT", "/v1/keys/k", e.tokenB, `{"value":"B"}`, idem("same-key"))
	e.expect(ra, 201, "")
	e.expect(rb, 201, "")
	if rb.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("tenant B was served tenant A's stored response")
	}
	for tok, want := range map[string]string{e.tokenA: `"A"`, e.tokenB: `"B"`} {
		if v, _ := valueOf(t, e.do("GET", "/v1/keys/k", tok, "", nil)); v != want {
			t.Fatalf("value = %s, want %s", v, want)
		}
	}
}

// Failed requests are not recorded, so a corrected retry executes.
func TestFailedRequestIsNotRecorded(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "k", `{"value":"v1"}`)
	h := idem("retry-1")
	h["If-Match"] = "9"
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"v2"}`, h), 412, "precondition_failed")

	h["If-Match"] = "1"
	r := e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"v2"}`, h)
	e.expect(r, 200, "")
	if r.header.Get("Idempotent-Replayed") != "" || r.header.Get("ETag") != `"2"` {
		t.Fatalf("retry = %+v", r)
	}
	// A 400 is not recorded either.
	e.expect(e.do("PUT", "/v1/keys/k2", e.tokenA, `{bad`, idem("bad-1")), 400, "invalid_request")
	e.expect(e.do("PUT", "/v1/keys/k2", e.tokenA, `{"value":1}`, idem("bad-1")), 201, "")
}

func TestInvalidIdempotencyKeyHeader(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":1}`, idem(strings.Repeat("x", 256))), 400, "invalid_request")
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":1}`, idem("has space")), 400, "invalid_request")
	e.expect(e.do("DELETE", "/v1/keys/k", e.tokenA, "", idem(strings.Repeat("x", 256))), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/keys/k", e.tokenA, "", nil), 404, "not_found")
	if n := e.changeCount(); n != 0 {
		t.Fatalf("rejected requests wrote %d changes", n)
	}
	// 255 is the max and is accepted.
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":1}`, idem(strings.Repeat("x", 255))), 201, "")
}
