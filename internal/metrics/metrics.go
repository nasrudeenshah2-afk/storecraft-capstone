// Package metrics is a small, dependency-free Prometheus text-format registry:
// counters, gauges and histograms (optionally labelled), safe for concurrent use.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// collector is anything that can render itself in the text exposition format.
type collector interface {
	write(w io.Writer) error
}

// Registry holds collectors in registration order.
type Registry struct {
	mu         sync.Mutex
	collectors []collector
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, c)
}

// WriteTo renders every collector in the Prometheus text format (0.0.4).
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	cs := append([]collector(nil), r.collectors...)
	r.mu.Unlock()
	cw := &countingWriter{w: w}
	for _, c := range cs {
		if err := c.write(cw); err != nil {
			return cw.n, err
		}
	}
	return cw.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func writeHeader(w io.Writer, name, help, typ string) error {
	_, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
	return err
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// labelString renders {a="x",b="y"} (or "" with no labels); extra is appended
// as a final pair (used for histogram "le").
func labelString(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, n, escapeLabelValue(values[i]))
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, extraName, escapeLabelValue(extraValue))
	}
	b.WriteByte('}')
	return b.String()
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// seriesKey joins label values into a map key. 0xff cannot appear in valid UTF-8.
func seriesKey(values []string) string { return strings.Join(values, "\xff") }

func splitKey(key string, n int) []string {
	if n == 0 {
		return nil
	}
	return strings.Split(key, "\xff")
}

// ---- Counter ----

// Counter is a monotonically increasing value.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

type counterMetric struct {
	name, help string
	c          Counter
}

func (m *counterMetric) write(w io.Writer) error {
	if err := writeHeader(w, m.name, m.help, "counter"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s %d\n", m.name, m.c.Value())
	return err
}

// NewCounter registers an unlabelled counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	m := &counterMetric{name: name, help: help}
	r.register(m)
	return &m.c
}

// CounterVec is a family of counters distinguished by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.RWMutex
	series     map[string]*Counter
}

func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	v := &CounterVec{name: name, help: help, labels: labels, series: map[string]*Counter{}}
	r.register(v)
	return v
}

// WithLabelValues returns (creating if needed) the counter for the values,
// given in the order the labels were declared.
func (v *CounterVec) WithLabelValues(values ...string) *Counter {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := seriesKey(values)
	v.mu.RLock()
	c, ok := v.series[key]
	v.mu.RUnlock()
	if ok {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok = v.series[key]; !ok {
		c = &Counter{}
		v.series[key] = c
	}
	return c
}

func (v *CounterVec) write(w io.Writer) error {
	if err := writeHeader(w, v.name, v.help, "counter"); err != nil {
		return err
	}
	v.mu.RLock()
	keys := make([]string, 0, len(v.series))
	vals := make(map[string]uint64, len(v.series))
	for k, c := range v.series {
		keys = append(keys, k)
		vals[k] = c.Value()
	}
	v.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s%s %d\n", v.name, labelString(v.labels, splitKey(k, len(v.labels)), "", ""), vals[k]); err != nil {
			return err
		}
	}
	return nil
}

// ---- Gauge ----

// Gauge is a value that can go up and down.
type Gauge struct{ v atomic.Int64 }

func (g *Gauge) Inc()         { g.v.Add(1) }
func (g *Gauge) Dec()         { g.v.Add(-1) }
func (g *Gauge) Set(n int64)  { g.v.Store(n) }
func (g *Gauge) Value() int64 { return g.v.Load() }

type gaugeMetric struct {
	name, help string
	g          Gauge
}

func (m *gaugeMetric) write(w io.Writer) error {
	if err := writeHeader(w, m.name, m.help, "gauge"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s %d\n", m.name, m.g.Value())
	return err
}

func (r *Registry) NewGauge(name, help string) *Gauge {
	m := &gaugeMetric{name: name, help: help}
	r.register(m)
	return &m.g
}

// ---- Histogram ----

// DefBuckets are upper bounds in seconds, from 1ms to 10s.
var DefBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram observes values into fixed buckets.
type Histogram struct {
	bounds []float64

	mu     sync.Mutex
	counts []uint64 // per bucket, non-cumulative; last slot is +Inf
	sum    float64
	count  uint64
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

// snapshot returns cumulative bucket counts (len(bounds)+1, last is +Inf), sum and count.
func (h *Histogram) snapshot() ([]uint64, float64, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cum := make([]uint64, len(h.counts))
	var run uint64
	for i, c := range h.counts {
		run += c
		cum[i] = run
	}
	return cum, h.sum, h.count
}

// HistogramVec is a family of histograms distinguished by label values.
type HistogramVec struct {
	name, help string
	labels     []string
	bounds     []float64
	mu         sync.RWMutex
	series     map[string]*Histogram
}

// NewHistogramVec registers a labelled histogram. buckets must be sorted
// ascending; nil means DefBuckets.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if buckets == nil {
		buckets = DefBuckets
	}
	if !sort.Float64sAreSorted(buckets) {
		panic("metrics: histogram buckets must be sorted ascending")
	}
	v := &HistogramVec{name: name, help: help, labels: labels, bounds: append([]float64(nil), buckets...), series: map[string]*Histogram{}}
	r.register(v)
	return v
}

func (v *HistogramVec) WithLabelValues(values ...string) *Histogram {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := seriesKey(values)
	v.mu.RLock()
	h, ok := v.series[key]
	v.mu.RUnlock()
	if ok {
		return h
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if h, ok = v.series[key]; !ok {
		h = &Histogram{bounds: v.bounds, counts: make([]uint64, len(v.bounds)+1)}
		v.series[key] = h
	}
	return h
}

func (v *HistogramVec) write(w io.Writer) error {
	if err := writeHeader(w, v.name, v.help, "histogram"); err != nil {
		return err
	}
	v.mu.RLock()
	keys := make([]string, 0, len(v.series))
	hs := make(map[string]*Histogram, len(v.series))
	for k, h := range v.series {
		keys = append(keys, k)
		hs[k] = h
	}
	v.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		values := splitKey(k, len(v.labels))
		cum, sum, count := hs[k].snapshot()
		for i, b := range v.bounds {
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", v.name, labelString(v.labels, values, "le", formatFloat(b)), cum[i]); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", v.name, labelString(v.labels, values, "le", "+Inf"), cum[len(v.bounds)]); err != nil {
			return err
		}
		ls := labelString(v.labels, values, "", "")
		if _, err := fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", v.name, ls, formatFloat(sum), v.name, ls, count); err != nil {
			return err
		}
	}
	return nil
}

// ---- Gauges read at scrape time ----

// Sample is one value of a FuncGauge, with its label values in declaration order.
type Sample struct {
	LabelValues []string
	Value       int64
}

type funcGauge struct {
	name, help string
	labels     []string
	fn         func() ([]Sample, error)
}

// NewFuncGauge registers a gauge (family) whose samples are produced by fn at
// every scrape, so the exposed value is never a stale copy. An error from fn
// fails the scrape.
func (r *Registry) NewFuncGauge(name, help string, labels []string, fn func() ([]Sample, error)) {
	r.register(&funcGauge{name: name, help: help, labels: labels, fn: fn})
}

func (g *funcGauge) write(w io.Writer) error {
	samples, err := g.fn()
	if err != nil {
		return fmt.Errorf("metrics: %s: %w", g.name, err)
	}
	if err := writeHeader(w, g.name, g.help, "gauge"); err != nil {
		return err
	}
	for _, s := range samples {
		if len(s.LabelValues) != len(g.labels) {
			return fmt.Errorf("metrics: %s expects %d label values, got %d", g.name, len(g.labels), len(s.LabelValues))
		}
		if _, err := fmt.Fprintf(w, "%s%s %d\n", g.name, labelString(g.labels, s.LabelValues, "", ""), s.Value); err != nil {
			return err
		}
	}
	return nil
}
