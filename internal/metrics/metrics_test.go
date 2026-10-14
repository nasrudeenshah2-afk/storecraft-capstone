package metrics_test

import (
	"strings"
	"sync"
	"testing"

	"storecraft/internal/metrics"
	"storecraft/internal/metrics/metricstest"
)

func render(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExactRendering(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.NewCounter("jobs_total", "Jobs done.")
	cv := r.NewCounterVec("req_total", `Requests with a \ and
newline in help.`, "method", "route")
	g := r.NewGauge("open", "Open things.")
	hv := r.NewHistogramVec("lat_seconds", "Latency.", []float64{0.1, 1}, "route")

	c.Add(3)
	cv.WithLabelValues("GET", "/a").Inc()
	cv.WithLabelValues("GET", "/a").Inc()
	cv.WithLabelValues("POST", `/we"ird\path`+"\n").Inc()
	g.Inc()
	g.Inc()
	g.Dec()
	hv.WithLabelValues("/a").Observe(0.05)
	hv.WithLabelValues("/a").Observe(0.5)
	hv.WithLabelValues("/a").Observe(7)

	want := `# HELP jobs_total Jobs done.
# TYPE jobs_total counter
jobs_total 3
# HELP req_total Requests with a \\ and\nnewline in help.
# TYPE req_total counter
req_total{method="GET",route="/a"} 2
req_total{method="POST",route="/we\"ird\\path\n"} 1
# HELP open Open things.
# TYPE open gauge
open 1
# HELP lat_seconds Latency.
# TYPE lat_seconds histogram
lat_seconds_bucket{route="/a",le="0.1"} 1
lat_seconds_bucket{route="/a",le="1"} 2
lat_seconds_bucket{route="/a",le="+Inf"} 3
lat_seconds_sum{route="/a"} 7.55
lat_seconds_count{route="/a"} 3
`
	got := render(t, r)
	if got != want {
		t.Fatalf("output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// It must also be valid exposition text, and escaping must round-trip.
	sc, err := metricstest.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := sc.Value("req_total", map[string]string{"method": "POST", "route": `/we"ird\path` + "\n"}); !ok || v != 1 {
		t.Fatalf("escaped label did not round-trip: %v %v", v, ok)
	}
}

func TestHistogramBoundaryAndInf(t *testing.T) {
	r := metrics.NewRegistry()
	hv := r.NewHistogramVec("h", "h", []float64{1, 2}, "k")
	h := hv.WithLabelValues("x")
	for _, v := range []float64{1, 1.0000001, 2, 3} { // le is inclusive
		h.Observe(v)
	}
	sc, err := metricstest.Parse(render(t, r))
	if err != nil {
		t.Fatal(err)
	}
	for le, want := range map[string]float64{"1": 1, "2": 3, "+Inf": 4} {
		if got := sc.MustValue("h_bucket", map[string]string{"k": "x", "le": le}); got != want {
			t.Errorf("le=%s: %v, want %v", le, got, want)
		}
	}
}

func TestStorecraftSetRendersValidWithKnownSeries(t *testing.T) {
	m := metrics.New()
	m.HTTPRequests.WithLabelValues("GET", "/v1/keys/{key}", "200").Inc()
	m.HTTPDuration.WithLabelValues("/v1/keys/{key}").Observe(0.002)

	sc, err := metricstest.Parse(render(t, m.Registry))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"storecraft_http_requests_total", "storecraft_http_request_duration_seconds",
		"storecraft_changes_written_total", "storecraft_idempotent_replays_total", "storecraft_sse_streams_active",
	} {
		if _, ok := sc.Types[name]; !ok {
			t.Errorf("missing metric family %s", name)
		}
	}
	// Known series exist at zero before any traffic.
	for _, op := range []string{"put", "delete"} {
		if v, ok := sc.Value("storecraft_changes_written_total", map[string]string{"op": op}); !ok || v != 0 {
			t.Errorf("changes_written{op=%s} = %v, %v; want 0, true", op, v, ok)
		}
	}
	if _, ok := sc.Value("storecraft_idempotent_replays_total", nil); !ok {
		t.Error("idempotent replays counter missing")
	}
	if _, ok := sc.Value("storecraft_sse_streams_active", nil); !ok {
		t.Error("sse gauge missing")
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	r := metrics.NewRegistry()
	cv := r.NewCounterVec("c_total", "c", "a", "b")
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for the wrong number of label values")
		}
	}()
	cv.WithLabelValues("only-one")
}

// Concurrent use is race-free and counts are exact; scrapes taken during
// the writes are always valid.
func TestConcurrentUse(t *testing.T) {
	m := metrics.New()
	const goroutines, perG = 64, 500
	routes := []string{"/a", "/b", "/c"}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	scrapeErr := make(chan error, 1)
	var scrapes sync.WaitGroup
	scrapes.Add(1)
	go func() {
		defer scrapes.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var b strings.Builder
			m.Registry.WriteTo(&b)
			if _, err := metricstest.Parse(b.String()); err != nil {
				select {
				case scrapeErr <- err:
				default:
				}
				return
			}
		}
	}()

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				route := routes[(g+i)%len(routes)]
				m.HTTPRequests.WithLabelValues("GET", route, "200").Inc()
				m.HTTPDuration.WithLabelValues(route).Observe(float64(i%50) / 1000)
				m.ChangesWritten.WithLabelValues("put").Inc()
				m.IdempotentReplays.Inc()
				m.SSEStreamsActive.Inc()
				m.SSEStreamsActive.Dec()
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	scrapes.Wait()
	select {
	case err := <-scrapeErr:
		t.Fatalf("invalid scrape during concurrent writes: %v", err)
	default:
	}

	var b strings.Builder
	m.Registry.WriteTo(&b)
	sc, err := metricstest.Parse(b.String())
	if err != nil {
		t.Fatal(err)
	}
	total := float64(goroutines * perG)
	if got := sc.Sum("storecraft_http_requests_total"); got != total {
		t.Errorf("requests_total = %v, want %v", got, total)
	}
	if got := sc.Sum("storecraft_http_request_duration_seconds_count"); got != total {
		t.Errorf("duration count = %v, want %v", got, total)
	}
	if got := sc.MustValue("storecraft_changes_written_total", map[string]string{"op": "put"}); got != total {
		t.Errorf("changes_written{put} = %v, want %v", got, total)
	}
	if got := sc.MustValue("storecraft_idempotent_replays_total", nil); got != total {
		t.Errorf("replays = %v, want %v", got, total)
	}
	if got := sc.MustValue("storecraft_sse_streams_active", nil); got != 0 {
		t.Errorf("gauge = %v, want 0", got)
	}
}
