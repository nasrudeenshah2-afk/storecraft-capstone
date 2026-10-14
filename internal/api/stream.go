package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"storecraft/internal/store"
)

// streamRoute serves GET /v1/changes/stream as Server-Sent Events. Everything
// that can fail with a normal JSON error (method, auth, parameters) is checked
// before the first stream byte is written.
func (s *server) streamRoute(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	since := int64(0)
	if r.URL.Query().Has("since") {
		n, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "since must be a non-negative integer")
			return
		}
		since = n
	}
	// Last-Event-ID (sent by EventSource on reconnect) wins over ?since=.
	if id := strings.TrimSpace(r.Header.Get("Last-Event-ID")); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "Last-Event-ID must be a non-negative integer")
			return
		}
		since = n
	}

	// Streams are long-lived: clear any server-wide write deadline.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Count the stream before the first byte goes out, so a client that has
	// seen the response headers always sees it in the gauge.
	s.m.SSEStreamsActive.Inc()
	defer s.m.SSEStreamsActive.Dec()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		s.log.Error("sse: response does not support flushing", "err", err)
		return
	}

	emit := func(changes []store.Change) error {
		var buf bytes.Buffer
		for _, c := range changes {
			data, err := marshal(toChangeJSON(c)) // compact: always one line
			if err != nil {
				return err
			}
			fmt.Fprintf(&buf, "id: %d\nevent: change\ndata: %s\n\n", c.Seq, data)
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		return rc.Flush()
	}
	ping := func() error {
		if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := s.feed.Stream(r.Context(), tenant.ID, since, s.heartbeat, emit, ping); err != nil {
		// Almost always the client going away mid-write; nothing to send.
		s.log.Debug("sse: stream ended", "err", err)
	}
}
