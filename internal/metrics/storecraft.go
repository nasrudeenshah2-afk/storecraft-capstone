package metrics

import "strconv"

// Metrics is the storecraft metric set. Names and help live here, in one
// place. No label ever carries a key, and only the two per-tenant usage gauges
// (STORE-210) carry a tenant id: their cardinality is bounded by the number of
// tenants.
type Metrics struct {
	Registry *Registry

	// HTTPRequests counts finished requests by method, route pattern and status.
	HTTPRequests *CounterVec
	// HTTPDuration observes request duration by route pattern.
	HTTPDuration *HistogramVec
	// ChangesWritten counts change-log rows written, by op ("put" or "delete").
	ChangesWritten *CounterVec
	// IdempotentReplays counts requests answered from a stored idempotent response.
	IdempotentReplays *Counter
	// SSEStreamsActive is the number of currently open SSE streams.
	SSEStreamsActive *Gauge
	// QuotaRejections counts 403 quota_exceeded responses by dimension ("keys" or "bytes").
	QuotaRejections *CounterVec
}

// New creates the metric set with every known series present at zero.
func New() *Metrics {
	r := NewRegistry()
	m := &Metrics{Registry: r}
	m.HTTPRequests = r.NewCounterVec("storecraft_http_requests_total",
		"Finished HTTP requests by method, route pattern and status code.",
		"method", "route", "status")
	m.HTTPDuration = r.NewHistogramVec("storecraft_http_request_duration_seconds",
		"HTTP request duration in seconds by route pattern. Long-poll and SSE requests are observed when they end.",
		nil, "route")
	m.ChangesWritten = r.NewCounterVec("storecraft_changes_written_total",
		"Change-log rows written, by operation.", "op")
	m.IdempotentReplays = r.NewCounter("storecraft_idempotent_replays_total",
		"Requests answered from a stored idempotent response instead of writing again.")
	m.SSEStreamsActive = r.NewGauge("storecraft_sse_streams_active",
		"Currently open Server-Sent Events change streams.")
	m.QuotaRejections = r.NewCounterVec("storecraft_quota_rejections_total",
		"Writes rejected with 403 quota_exceeded, by the quota dimension that was exceeded (keys or bytes).", "dimension")

	m.ChangesWritten.WithLabelValues("put")
	m.ChangesWritten.WithLabelValues("delete")
	m.QuotaRejections.WithLabelValues("keys")
	m.QuotaRejections.WithLabelValues("bytes")
	return m
}

// TenantUsage is one tenant's usage, as reported by the usage gauges.
type TenantUsage struct {
	TenantID int64
	Keys     int64
	Bytes    int64
}

// AddQuotaGauges registers the STORE-210 gauges. Their values are read at
// scrape time from the given functions (the database), never cached:
// storecraft_tenant_usage_keys / _bytes per tenant_id, and
// storecraft_quota_enforcement_enabled (1 when the flag is on, else 0).
func (m *Metrics) AddQuotaGauges(usage func() ([]TenantUsage, error), enforcement func() (bool, error)) {
	read := func(pick func(TenantUsage) int64) func() ([]Sample, error) {
		return func() ([]Sample, error) {
			us, err := usage()
			if err != nil {
				return nil, err
			}
			out := make([]Sample, 0, len(us))
			for _, u := range us {
				out = append(out, Sample{LabelValues: []string{strconv.FormatInt(u.TenantID, 10)}, Value: pick(u)})
			}
			return out, nil
		}
	}
	m.Registry.NewFuncGauge("storecraft_tenant_usage_keys",
		"Keys stored per tenant (STORE-210 usage). tenant_id is bounded by the number of tenants.",
		[]string{"tenant_id"}, read(func(u TenantUsage) int64 { return u.Keys }))
	m.Registry.NewFuncGauge("storecraft_tenant_usage_bytes",
		"Value bytes (canonical JSON) stored per tenant (STORE-210 usage). tenant_id is bounded by the number of tenants.",
		[]string{"tenant_id"}, read(func(u TenantUsage) int64 { return u.Bytes }))
	m.Registry.NewFuncGauge("storecraft_quota_enforcement_enabled",
		"1 when the quota_enforcement flag is on (writes over quota are rejected), else 0.",
		nil, func() ([]Sample, error) {
			on, err := enforcement()
			if err != nil {
				return nil, err
			}
			v := int64(0)
			if on {
				v = 1
			}
			return []Sample{{Value: v}}, nil
		})
}
