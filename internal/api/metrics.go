package api

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/metrics"
	"storecraft/internal/store"
)

// statusWriter records the response status. Unwrap lets
// http.NewResponseController (used for SSE flushing and deadlines) reach the
// underlying writer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 { // ignore 1xx informational headers
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// methodLabel keeps client-supplied methods from becoming label values.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "OTHER"
}

// instrument records the request count and duration under the route's
// documented pattern (never the raw path). Long-lived requests (long-poll,
// SSE) are recorded when they end. A panic is recorded as a 500 and re-raised
// for the recoverer.
func (s *server) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			status := sw.status
			if status == 0 {
				status = http.StatusOK
				if p != nil {
					status = http.StatusInternalServerError
				}
			}
			s.m.HTTPRequests.WithLabelValues(methodLabel(r.Method), route, strconv.Itoa(status)).Inc()
			s.m.HTTPDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
			if p != nil {
				panic(p)
			}
		}()
		h(sw, r)
	}
}

// scrapeTimeout bounds the database reads made while rendering /metrics.
const scrapeTimeout = 5 * time.Second

// scrapeUsage reads every tenant's usage for the usage gauges, at scrape time.
func (s *server) scrapeUsage() ([]metrics.TenantUsage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	us, err := s.st.AllUsage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]metrics.TenantUsage, 0, len(us))
	for _, u := range us {
		out = append(out, metrics.TenantUsage{TenantID: u.TenantID, Keys: u.Usage.Keys, Bytes: u.Usage.Bytes})
	}
	return out, nil
}

// scrapeEnforcement reads the quota_enforcement flag for its gauge, at scrape time.
func (s *server) scrapeEnforcement() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	f, err := s.st.Flag(ctx, store.FlagQuotaEnforcement)
	return f.Enabled, err
}

// metricsRoute serves GET /metrics. Only the admin token may read it; anything
// else (no token, a wrong token, or a tenant token) is a 401.
func (s *server) metricsRoute(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok || !s.isAdminHash(auth.HashToken(token)) {
		writeUnauthorized(w)
		return
	}
	var buf bytes.Buffer
	if _, err := s.m.Registry.WriteTo(&buf); err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(buf.Bytes())
}
