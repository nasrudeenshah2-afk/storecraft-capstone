package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type sseEvent struct{ ID, Event, Data string }

// sseClient is a minimal real SSE client over a real HTTP connection.
type sseClient struct {
	t        *testing.T
	cancel   context.CancelFunc
	events   chan sseEvent
	comments chan string
}

// stream opens GET /v1/changes/stream and requires a 200 text/event-stream.
func (e *env) stream(token, query string, hdr map[string]string) *sseClient {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", e.url+"/v1/changes/stream"+query, nil)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		b := make([]byte, 300)
		n, _ := res.Body.Read(b)
		cancel()
		e.t.Fatalf("stream: status %d content-type %q body %q", res.StatusCode, res.Header.Get("Content-Type"), b[:n])
	}
	c := &sseClient{t: e.t, cancel: cancel, events: make(chan sseEvent, 4096), comments: make(chan string, 4096)}
	go func() {
		defer res.Body.Close()
		defer close(c.events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64*1024), 2<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					c.events <- ev
					ev = sseEvent{}
				}
			case strings.HasPrefix(line, ":"):
				c.comments <- strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.Data = line[6:]
			}
		}
	}()
	e.t.Cleanup(cancel)
	return c
}

func (c *sseClient) close() { c.cancel() }

// next returns the next event or fails the test after timeout.
func (c *sseClient) next(timeout time.Duration) sseEvent {
	c.t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			c.t.Fatal("stream closed while waiting for an event")
		}
		return ev
	case <-time.After(timeout):
		c.t.Fatalf("no event within %v", timeout)
	}
	return sseEvent{}
}

// expectNone fails if an event arrives within d (comments are fine).
func (c *sseClient) expectNone(d time.Duration) {
	c.t.Helper()
	select {
	case ev, ok := <-c.events:
		if ok {
			c.t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(d):
	}
}

func (ev sseEvent) seq(t *testing.T) int64 {
	t.Helper()
	n, err := strconv.ParseInt(ev.ID, 10, 64)
	if err != nil {
		t.Fatalf("bad event id %q", ev.ID)
	}
	return n
}

func (ev sseEvent) key(t *testing.T) string {
	t.Helper()
	var d struct {
		Seq int64  `json:"seq"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		t.Fatalf("bad event data %q: %v", ev.Data, err)
	}
	if d.Seq != ev.seq(t) {
		t.Fatalf("data.seq %d != id %s", d.Seq, ev.ID)
	}
	return d.Key
}

func fastHeartbeat(t *testing.T) *env {
	return setupFull(t, Options{SSEHeartbeat: 100 * time.Millisecond}, nil)
}

// (a) Events arrive in seq order after writes.
func TestSSEDeliversEventsInSeqOrder(t *testing.T) {
	e := fastHeartbeat(t)
	c := e.stream(e.tokenA, "", nil)
	e.putConcurrently(20, func(i int) string { return fmt.Sprintf("k%02d", i) })

	var prev int64
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		ev := c.next(5 * time.Second)
		if ev.Event != "change" {
			t.Fatalf("event type = %q", ev.Event)
		}
		if i > 0 && ev.seq(t) != prev+1 {
			t.Fatalf("event %d id %d after %d: gap, duplicate or out of order", i, ev.seq(t), prev)
		}
		prev = ev.seq(t)
		if seen[ev.key(t)] {
			t.Fatalf("duplicate key %s", ev.key(t))
		}
		seen[ev.key(t)] = true
		if !strings.Contains(ev.Data, `"op":"put"`) || strings.Contains(ev.Data, "\n") {
			t.Fatalf("bad data %q", ev.Data)
		}
	}
	c.expectNone(300 * time.Millisecond)
}

// A backlog that exists before connecting is delivered first, from ?since=.
func TestSSESinceQuery(t *testing.T) {
	e := fastHeartbeat(t)
	for i := 1; i <= 5; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%d", i), `{"value":1}`)
	}
	// Tenant creation also takes seqs, so take the cursor from the real log.
	third := e.changes(e.tokenA, "?limit=500").Changes[2].Seq
	c := e.stream(e.tokenA, fmt.Sprintf("?since=%d", third), nil)
	for _, want := range []string{"k4", "k5"} {
		if got := c.next(5 * time.Second).key(t); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	c.expectNone(300 * time.Millisecond)
}

// (b) Reconnect with Last-Event-ID: no duplicates, no gaps.
func TestSSEReconnectWithLastEventID(t *testing.T) {
	e := fastHeartbeat(t)
	c := e.stream(e.tokenA, "", nil)
	for i := 1; i <= 5; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%d", i), `{"value":1}`)
	}
	var last string
	for i := 1; i <= 5; i++ {
		ev := c.next(5 * time.Second)
		if ev.key(t) != fmt.Sprintf("k%d", i) {
			t.Fatalf("event %d key %s", i, ev.key(t))
		}
		last = ev.ID
	}
	c.close()

	// Writes while disconnected.
	for i := 6; i <= 8; i++ {
		e.put(e.tokenA, fmt.Sprintf("k%d", i), `{"value":1}`)
	}

	c2 := e.stream(e.tokenA, "", map[string]string{"Last-Event-ID": last})
	lastSeq, _ := strconv.ParseInt(last, 10, 64)
	for i := 6; i <= 8; i++ {
		ev := c2.next(5 * time.Second)
		if ev.key(t) != fmt.Sprintf("k%d", i) || ev.seq(t) != lastSeq+int64(i-5) {
			t.Fatalf("after reconnect: got %s id %s, want k%d id %d", ev.key(t), ev.ID, i, lastSeq+int64(i-5))
		}
	}
	// Live write continues the same sequence; nothing is repeated.
	e.put(e.tokenA, "k9", `{"value":1}`)
	if ev := c2.next(5 * time.Second); ev.key(t) != "k9" || ev.seq(t) != lastSeq+4 {
		t.Fatalf("live event = %+v", ev)
	}
	c2.expectNone(300 * time.Millisecond)

	// Last-Event-ID wins over ?since=.
	all := e.changes(e.tokenA, "?limit=500")
	seventh := all.Changes[6].Seq
	c3 := e.stream(e.tokenA, "?since=0", map[string]string{"Last-Event-ID": strconv.FormatInt(seventh, 10)})
	if ev := c3.next(5 * time.Second); ev.key(t) != "k8" {
		t.Fatalf("header should win over since: first event %s, want k8", ev.key(t))
	}
}

// (c) A stream only ever carries its own tenant's changes.
func TestSSETenantIsolation(t *testing.T) {
	e := fastHeartbeat(t)
	a := e.stream(e.tokenA, "", nil)
	b := e.stream(e.tokenB, "", nil)

	e.put(e.tokenA, "a-secret", `{"value":"A"}`)
	if got := a.next(5 * time.Second).key(t); got != "a-secret" {
		t.Fatalf("A got %s", got)
	}
	b.expectNone(500 * time.Millisecond)

	e.put(e.tokenB, "b-key", `{"value":"B"}`)
	if got := b.next(5 * time.Second).key(t); got != "b-key" {
		t.Fatalf("B got %s", got)
	}
	a.expectNone(300 * time.Millisecond)

	// Backlog replay is scoped too.
	late := e.stream(e.tokenB, "?since=0", nil)
	if got := late.next(5 * time.Second).key(t); got != "b-key" {
		t.Fatalf("B backlog first event %s", got)
	}
	late.expectNone(300 * time.Millisecond)
}

func TestSSEHeartbeat(t *testing.T) {
	e := fastHeartbeat(t)
	c := e.stream(e.tokenA, "", nil)
	deadline := time.After(3 * time.Second)
	for n := 0; n < 3; n++ {
		select {
		case cm := <-c.comments:
			if cm != "heartbeat" {
				t.Fatalf("comment = %q", cm)
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats", n)
		}
	}
}

func TestSSEErrorsAreJSONEnvelopes(t *testing.T) {
	e := fastHeartbeat(t)
	e.expect(e.do("GET", "/v1/changes/stream", "", "", nil), 401, "unauthorized")
	e.expect(e.do("GET", "/v1/changes/stream", "wrong", "", nil), 401, "unauthorized")
	e.expect(e.do("GET", "/v1/changes/stream", adminToken, "", nil), 403, "forbidden")
	e.expect(e.do("GET", "/v1/changes/stream?since=abc", e.tokenA, "", nil), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/changes/stream?since=-1", e.tokenA, "", nil), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/changes/stream", e.tokenA, "", map[string]string{"Last-Event-ID": "nope"}), 400, "invalid_request")
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		r := e.do(m, "/v1/changes/stream", e.tokenA, `{"value":1}`, nil)
		e.expect(r, 405, "method_not_allowed")
		if r.header.Get("Allow") != "GET" {
			t.Fatalf("%s Allow = %q", m, r.header.Get("Allow"))
		}
	}
}

// A server-wide WriteTimeout must not kill a stream.
func TestSSESurvivesServerWriteTimeout(t *testing.T) {
	e := setupFull(t, Options{SSEHeartbeat: 100 * time.Millisecond}, func(s *http.Server) {
		s.WriteTimeout = 300 * time.Millisecond
	})
	c := e.stream(e.tokenA, "", nil)

	time.Sleep(1200 * time.Millisecond) // 4x the write timeout
	e.put(e.tokenA, "late", `{"value":1}`)
	if got := c.next(5 * time.Second).key(t); got != "late" {
		t.Fatalf("event after write timeout window: %s", got)
	}
	select {
	case <-c.comments: // heartbeats kept flowing too
	default:
		t.Fatal("no heartbeats received")
	}
}

// The handler returns when the client disconnects.
func TestSSEStopsOnClientDisconnect(t *testing.T) {
	e := fastHeartbeat(t)
	streams := []*sseClient{
		e.stream(e.tokenA, "", nil),
		e.stream(e.tokenA, "", nil),
		e.stream(e.tokenB, "", nil),
	}
	e.put(e.tokenA, "x", `{"value":1}`)
	streams[0].next(5 * time.Second)
	for _, s := range streams {
		s.close()
	}
	done := make(chan struct{})
	go func() {
		e.srv.Close() // blocks until every handler has returned
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server still has stream handlers running after clients disconnected")
	}
}
