package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/metrics/metricstest"
)

// scrape reads /metrics with the admin token and validates the output.
func (e *env) scrape() (string, *metricstest.Scrape) {
	e.t.Helper()
	r := e.do("GET", "/metrics", adminToken, "", nil)
	if r.status != 200 {
		e.t.Fatalf("/metrics status %d: %s", r.status, r.body)
	}
	sc, err := metricstest.Parse(r.body)
	if err != nil {
		e.t.Fatalf("/metrics output is not valid Prometheus text: %v\n%s", err, r.body)
	}
	return r.body, sc
}

func reqLabels(method, route, status string) map[string]string {
	return map[string]string{"method": method, "route": route, "status": status}
}

const (
	reqTotal = "storecraft_http_requests_total"
	durCount = "storecraft_http_request_duration_seconds_count"
	changes  = "storecraft_changes_written_total"
)

func TestMetricsRequireAdminToken(t *testing.T) {
	e := setup(t)
	for name, token := range map[string]string{"none": "", "wrong": "nope", "tenant": e.tokenA} {
		r := e.do("GET", "/metrics", token, "", nil)
		if r.status != 401 {
			t.Fatalf("%s token: status %d, want 401", name, r.status)
		}
		e.checkEnvelope(r, "unauthorized")
		if r.header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s: WWW-Authenticate = %q", name, r.header.Get("WWW-Authenticate"))
		}
	}
	r := e.do("GET", "/metrics", adminToken, "", nil)
	e.expect(r, 200, "")
	if ct := r.header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(r.body, "# TYPE storecraft_http_requests_total counter") {
		t.Fatalf("unexpected body: %s", r.body)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		x := e.do(m, "/metrics", adminToken, "", nil)
		e.expect(x, 405, "method_not_allowed")
		if x.header.Get("Allow") != "GET" {
			t.Fatalf("Allow = %q", x.header.Get("Allow"))
		}
	}
}

func TestMetricsDisabledWithoutAdminToken(t *testing.T) {
	e := setupFull(t, Options{AdminToken: noAdmin}, nil)
	for _, token := range []string{"", "anything", adminToken, e.tokenA} {
		e.expect(e.do("GET", "/metrics", token, "", nil), 401, "unauthorized")
	}
}

func TestMetricsCountersIncreaseAfterRequests(t *testing.T) {
	e := setup(t)
	_, before := e.scrape()

	for i := 0; i < 3; i++ {
		e.expect(e.do("GET", "/v1/keys/x", e.tokenA, "", nil), 404, "")
	}
	e.expect(e.do("PUT", "/v1/keys/x", e.tokenA, `{"value":1}`, nil), 201, "")
	e.expect(e.do("PUT", "/v1/keys/x", e.tokenA, `{"value":2}`, nil), 200, "")
	e.expect(e.do("DELETE", "/v1/keys/x", e.tokenA, "", nil), 204, "")
	e.expect(e.do("GET", "/v1/keys", e.tokenA, "", nil), 200, "")

	_, after := e.scrape()
	delta := func(name string, labels map[string]string) float64 {
		return after.MustValue(name, labels) - before.MustValue(name, labels)
	}
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{reqTotal, reqLabels("GET", "/v1/keys/{key}", "404"), 3},
		{reqTotal, reqLabels("PUT", "/v1/keys/{key}", "201"), 1},
		{reqTotal, reqLabels("PUT", "/v1/keys/{key}", "200"), 1},
		{reqTotal, reqLabels("DELETE", "/v1/keys/{key}", "204"), 1},
		{reqTotal, reqLabels("GET", "/v1/keys", "200"), 1},
		{reqTotal, reqLabels("GET", "/metrics", "200"), 1}, // the first scrape
		{durCount, map[string]string{"route": "/v1/keys/{key}"}, 6},
		{durCount, map[string]string{"route": "/v1/keys"}, 1},
		{changes, map[string]string{"op": "put"}, 2},
		{changes, map[string]string{"op": "delete"}, 1},
	}
	for _, c := range checks {
		if got := delta(c.name, c.labels); got != c.want {
			t.Errorf("%s%v increased by %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if s := after.MustValue("storecraft_http_request_duration_seconds_sum", map[string]string{"route": "/v1/keys/{key}"}); s <= 0 {
		t.Errorf("duration sum = %v, want > 0", s)
	}
}

func TestMetricsLabelsUseRoutePatternNotRawPath(t *testing.T) {
	e := setup(t)
	for _, k := range []string{"aaa-secret", "bbb-secret", "ccc/with/slashes"} {
		e.expect(e.do("GET", "/v1/keys/"+k, e.tokenA, "", nil), 404, "")
	}
	e.expect(e.do("GET", "/totally/unknown/path-12345", e.tokenA, "", nil), 404, "")
	e.expect(e.do("GET", "/another/zzz-67890", "", "", nil), 404, "")
	e.expect(e.do("FOOBAR", "/v1/keys/aaa-secret", e.tokenA, "", nil), 405, "")

	text, sc := e.scrape()
	for _, raw := range []string{"aaa-secret", "bbb-secret", "ccc", "path-12345", "zzz-67890", "unknown", "slashes"} {
		if strings.Contains(text, raw) {
			t.Errorf("raw path fragment %q leaked into the metrics output", raw)
		}
	}
	if got := sc.MustValue(reqTotal, reqLabels("GET", "/v1/keys/{key}", "404")); got != 3 {
		t.Errorf("GET /v1/keys/{key} 404 = %v, want 3 (one series for all keys)", got)
	}
	if got := sc.MustValue(reqTotal, reqLabels("GET", "unmatched", "404")); got != 2 {
		t.Errorf("unmatched 404 = %v, want 2", got)
	}
	if got := sc.MustValue(reqTotal, reqLabels("OTHER", "/v1/keys/{key}", "405")); got != 1 {
		t.Errorf("unknown method label = %v, want it folded into OTHER", got)
	}

	// Every route label is a documented pattern or "unmatched".
	allowed := map[string]bool{unmatchedRoute: true}
	for _, r := range Routes() {
		allowed[r.Path] = true
	}
	// The only exception to "no tenant labels" (STORE-210, spec section 9): the
	// two per-tenant usage gauges carry tenant_id - and nothing else does.
	tenantGauges := map[string]bool{"storecraft_tenant_usage_keys": true, "storecraft_tenant_usage_bytes": true}
	for _, sm := range sc.Samples {
		if route, ok := sm.Labels["route"]; ok && !allowed[route] {
			t.Errorf("%s has route label %q, not a route pattern", sm.Name, route)
		}
		for _, banned := range []string{"tenant", "tenant_id", "key"} {
			if _, ok := sm.Labels[banned]; ok && !(banned == "tenant_id" && tenantGauges[sm.Name]) {
				t.Errorf("%s carries a %q label", sm.Name, banned)
			}
		}
		if _, ok := sm.Labels["tenant_id"]; tenantGauges[sm.Name] && (!ok || len(sm.Labels) != 1) {
			t.Errorf("%s labels = %v, want exactly tenant_id", sm.Name, sm.Labels)
		}
	}
}

func TestMetricsHistogramOutputIsValid(t *testing.T) {
	e := setup(t)
	e.put(e.tokenA, "k", `{"value":1}`)
	e.expect(e.do("GET", "/v1/keys/k", e.tokenA, "", nil), 200, "")
	e.expect(e.do("GET", "/nope", "", "", nil), 404, "")

	_, sc := e.scrape() // scrape() already runs the full format validation
	if sc.Types["storecraft_http_request_duration_seconds"] != "histogram" {
		t.Fatalf("type = %q", sc.Types["storecraft_http_request_duration_seconds"])
	}
	wantLE := []string{"0.001", "0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"}
	for _, route := range []string{"/v1/keys/{key}", unmatchedRoute} {
		for _, le := range wantLE {
			if _, ok := sc.Value("storecraft_http_request_duration_seconds_bucket", map[string]string{"route": route, "le": le}); !ok {
				t.Errorf("route %s: missing bucket le=%s", route, le)
			}
		}
	}
	// With nothing in flight the histogram count equals the request counter, per route.
	perRoute := map[string]float64{}
	for _, sm := range sc.Samples {
		if sm.Name == reqTotal {
			perRoute[sm.Labels["route"]] += sm.Value
		}
	}
	for route, n := range perRoute {
		if route == "/metrics" {
			continue // the in-progress scrape is not counted yet; checked after
		}
		if got := sc.MustValue(durCount, map[string]string{"route": route}); got != n {
			t.Errorf("route %s: histogram count %v != request counter %v", route, got, n)
		}
	}
}

func TestMetricsChangesAndReplayCounters(t *testing.T) {
	e := setup(t)
	_, base := e.scrape()
	val := func(sc *metricstest.Scrape, name string, l map[string]string) float64 { return sc.MustValue(name, l) }

	// Two requests with one Idempotency-Key: one write, one replay.
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":1}`, idem("same")), 201, "")
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":1}`, idem("same")), 201, "")
	// None of these write, so none may count as a written change.
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":2}`, map[string]string{"If-Match": "9"}), 412, "")
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{bad`, nil), 400, "")
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":2}`, idem("same")), 422, "")
	e.expect(e.do("DELETE", "/v1/keys/ghost", e.tokenA, "", nil), 404, "")

	_, sc := e.scrape()
	if d := val(sc, "storecraft_idempotent_replays_total", nil) - val(base, "storecraft_idempotent_replays_total", nil); d != 1 {
		t.Errorf("replays +%v, want +1", d)
	}
	if d := val(sc, changes, map[string]string{"op": "put"}) - val(base, changes, map[string]string{"op": "put"}); d != 1 {
		t.Errorf("changes_written{put} +%v, want +1", d)
	}
	if d := val(sc, changes, map[string]string{"op": "delete"}) - val(base, changes, map[string]string{"op": "delete"}); d != 0 {
		t.Errorf("changes_written{delete} +%v, want +0", d)
	}
	// The counter agrees with the change log itself.
	e.expect(e.do("DELETE", "/v1/keys/k", e.tokenA, "", nil), 204, "")
	_, sc = e.scrape()
	log := e.changes(e.tokenA, "?since=0&limit=500")
	if got, want := sc.Sum(changes)-base.Sum(changes), float64(len(log.Changes)); got != want {
		t.Errorf("changes_written total +%v, change log has %v rows", got, want)
	}
}

func TestMetricsSSEStreamsActiveGauge(t *testing.T) {
	e := fastHeartbeat(t)
	gauge := func() float64 {
		_, sc := e.scrape()
		return sc.MustValue("storecraft_sse_streams_active", nil)
	}
	waitFor := func(want float64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if g := gauge(); g == want {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("sse_streams_active = %v, want %v", g, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if g := gauge(); g != 0 {
		t.Fatalf("initial gauge = %v", g)
	}
	_, before := e.scrape()

	a := e.stream(e.tokenA, "", nil)
	b := e.stream(e.tokenB, "", nil)
	waitFor(2)
	a.close()
	waitFor(1)
	b.close()
	waitFor(0)

	// Ended streams are recorded as finished requests (status 200).
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, sc := e.scrape()
		got := sc.MustValue(reqTotal, reqLabels("GET", "/v1/changes/stream", "200")) - before.MustValue(reqTotal, reqLabels("GET", "/v1/changes/stream", "200"))
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finished streams counted = %v, want 2", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Concurrent requests (and concurrent scrapes) are race-free and every request is counted exactly once.
func TestMetricsConcurrentRequests(t *testing.T) {
	e := setup(t)
	const goroutines, iterations = 40, 10

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: goroutines + 5}, Timeout: 60 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	send := func(method, path, token, body string) int {
		req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			return -1
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}

	stop := make(chan struct{})
	scraped := make(chan error, 1)
	var scrapeWG sync.WaitGroup
	scrapeWG.Add(1)
	go func() { // scrape continuously while the load runs
		defer scrapeWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, _ := http.NewRequest("GET", e.url+"/metrics", nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			res, err := client.Do(req)
			if err != nil {
				scraped <- err
				return
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if _, err := metricstest.Parse(string(b)); err != nil {
				scraped <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	var bad sync.Map
	expect := func(got, want int, what string) {
		if got != want {
			bad.Store(what, fmt.Sprintf("got %d, want %d", got, want))
		}
	}
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				k := fmt.Sprintf("g%d-i%d", g, i)
				expect(send("GET", "/v1/keys/"+k, e.tokenA, ""), 404, "get-missing")
				expect(send("PUT", "/v1/keys/"+k, e.tokenA, `{"value":1}`), 201, "put")
				expect(send("GET", "/v1/keys", e.tokenA, ""), 200, "list")
				expect(send("GET", "/no/such/"+k, e.tokenA, ""), 404, "unmatched")
				expect(send("GET", "/v1/keys/"+k, "", ""), 401, "no-token")
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	scrapeWG.Wait()
	select {
	case err := <-scraped:
		t.Fatalf("scrape during load failed: %v", err)
	default:
	}
	bad.Range(func(k, v any) bool { t.Errorf("%v: %v", k, v); return true })

	_, sc := e.scrape()
	n := float64(goroutines * iterations)
	for _, c := range []struct {
		labels map[string]string
		want   float64
	}{
		{reqLabels("GET", "/v1/keys/{key}", "404"), n},
		{reqLabels("PUT", "/v1/keys/{key}", "201"), n},
		{reqLabels("GET", "/v1/keys", "200"), n},
		{reqLabels("GET", "unmatched", "404"), n},
		{reqLabels("GET", "/v1/keys/{key}", "401"), n},
	} {
		if got := sc.MustValue(reqTotal, c.labels); got != c.want {
			t.Errorf("%v = %v, want exactly %v", c.labels, got, c.want)
		}
	}
	if got := sc.MustValue(changes, map[string]string{"op": "put"}); got != n {
		t.Errorf("changes_written{put} = %v, want %v", got, n)
	}
	if got := sc.MustValue(durCount, map[string]string{"route": "/v1/keys/{key}"}); got != 3*n {
		t.Errorf("key-route histogram count = %v, want %v", got, 3*n)
	}
}
