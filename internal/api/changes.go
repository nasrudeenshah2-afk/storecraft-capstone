package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"storecraft/internal/store"
)

type changeJSON struct {
	Seq       int64           `json:"seq"`
	Key       string          `json:"key"`
	Op        string          `json:"op"`
	Version   int64           `json:"version"`
	Value     json.RawMessage `json:"value"` // null for deletes
	ValueSize int64           `json:"value_size"`
	TS        string          `json:"ts"`
}

// changesRoute serves GET /v1/changes?since=N&limit=M, long-polling when
// there is nothing newer than since.
func (s *server) changesRoute(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	since, limit, ok := parseSinceLimit(w, r)
	if !ok {
		return
	}

	changes, err := s.feed.Poll(r.Context(), tenant.ID, since, limit, s.pollTimeout)
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away; nothing to send
		}
		s.internal(w, r, err)
		return
	}
	out := make([]changeJSON, 0, len(changes))
	last := since
	for _, c := range changes {
		out = append(out, toChangeJSON(c))
		last = c.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "last_seq": last})
}

// parseSinceLimit reads the shared ?since= and ?limit= parameters. On a bad
// value it writes a 400 and returns ok=false. Limits above the maximum are
// clamped to it.
func parseSinceLimit(w http.ResponseWriter, r *http.Request) (since int64, limit int, ok bool) {
	q := r.URL.Query()
	if q.Has("since") {
		n, err := strconv.ParseInt(q.Get("since"), 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "since must be a non-negative integer")
			return 0, 0, false
		}
		since = n
	}
	limit = defaultChangeLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		limit = min(n, maxChangeLimit)
	}
	return since, limit, true
}

func toChangeJSON(c store.Change) changeJSON {
	cj := changeJSON{
		Seq: c.Seq, Key: c.Key, Op: c.Op, Version: c.Version, ValueSize: c.ValueSize,
		TS: c.TS.Format(time.RFC3339Nano),
	}
	if c.Value != nil {
		cj.Value = c.Value
	}
	return cj
}
