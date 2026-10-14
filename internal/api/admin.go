package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

// Node roles.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
)

// ReplicationSource is what a replica node reports about its primary.
type ReplicationSource interface {
	// PrimarySeq is the primary's newest change seq, as last reported by the primary.
	PrimarySeq() int64
}

// requireAdmin lets only the admin token through. A missing, malformed or
// unknown token is a 401; a valid tenant token is a 403 (authenticated, but
// not allowed here).
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return false
	}
	hash := auth.HashToken(token)
	if s.isAdminHash(hash) {
		return true
	}
	if _, err := s.st.TenantByTokenHash(r.Context(), hash); err == nil {
		writeError(w, http.StatusForbidden, codeForbidden, "this route requires the admin token")
		return false
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, r, err)
		return false
	}
	writeUnauthorized(w)
	return false
}

type adminChangeJSON struct {
	Seq       int64           `json:"seq"`
	TenantID  int64           `json:"tenant_id"`
	Kind      string          `json:"kind"` // "key" or "tenant"
	Key       string          `json:"key"`  // for kind "tenant": the tenant name
	Op        string          `json:"op"`
	Version   int64           `json:"version"`
	Value     json.RawMessage `json:"value"` // null for deletes; for "tenant": {"name","token_hash"}, or the quota when subject is "quota"
	ValueSize int64           `json:"value_size"`
	Subject   *string         `json:"subject,omitempty"` // "quota" for a quota change; absent for tenant creation and keys
	TS        string          `json:"ts"`
}

// adminChangesRoute serves GET /v1/admin/changes?since=N&limit=M: every
// tenant's changes, including tenant creation and quota changes, in seq
// order, with the same long-poll behavior as /v1/changes. latest_seq is this
// node's newest seq, which replicas use to measure lag. flags carries the
// current runtime flags (STORE-210); a replica that sends the flag version it
// holds (?flags_version=N) is answered at once when that is out of date, so a
// flag change reaches it without waiting for the poll timeout.
func (s *server) adminChangesRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	since, limit, ok := parseSinceLimit(w, r)
	if !ok {
		return
	}
	var flagStale func(context.Context) (bool, error)
	if q := r.URL.Query(); q.Has("flags_version") {
		known, err := strconv.ParseInt(q.Get("flags_version"), 10, 64)
		if err != nil || known < 0 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "flags_version must be a non-negative integer")
			return
		}
		flagStale = func(ctx context.Context) (bool, error) {
			f, err := s.st.Flag(ctx, store.FlagQuotaEnforcement)
			return f.Version != known, err
		}
	}
	changes, err := s.feed.PollAllUntil(r.Context(), since, limit, s.pollTimeout, flagStale)
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away; nothing to send
		}
		s.internal(w, r, err)
		return
	}
	latest, err := s.st.LatestSeq(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	flag, err := s.st.Flag(r.Context(), store.FlagQuotaEnforcement)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]adminChangeJSON, 0, len(changes))
	last := since
	for _, c := range changes {
		cj := adminChangeJSON{
			Seq: c.Seq, TenantID: c.TenantID, Kind: c.Kind, Key: c.Key, Op: c.Op,
			Version: c.Version, ValueSize: c.ValueSize, TS: c.TS.Format(time.RFC3339Nano),
		}
		if c.Value != nil {
			cj.Value = c.Value
		}
		if c.Subject != "" {
			subject := c.Subject
			cj.Subject = &subject
		}
		out = append(out, cj)
		last = c.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "last_seq": last, "latest_seq": latest,
		"flags": flagsJSON{QuotaEnforcement: flagJSON{Enabled: flag.Enabled, Version: flag.Version}}})
}

// replicationStatusRoute serves GET /v1/replication/status. On a primary
// applied_seq and primary_seq are both its newest seq (lag 0). On a replica,
// applied_seq is the primary's seq of the last applied change and primary_seq
// the newest primary seq the primary has reported.
func (s *server) replicationStatusRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var applied, primary int64
	if s.readOnly {
		var err error
		if applied, err = s.st.AppliedSeq(r.Context()); err != nil {
			s.internal(w, r, err)
			return
		}
		if s.replica != nil {
			primary = s.replica.PrimarySeq()
		}
		primary = max(primary, applied)
	} else {
		latest, err := s.st.LatestSeq(r.Context())
		if err != nil {
			s.internal(w, r, err)
			return
		}
		applied, primary = latest, latest
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"role":        s.role,
		"applied_seq": applied,
		"primary_seq": primary,
		"lag":         primary - applied,
	})
}

type digestSummaryJSON struct {
	TenantID   int64           `json:"tenant_id"`
	Name       string          `json:"name"`
	Keys       int64           `json:"keys"`
	ValueBytes int64           `json:"value_bytes"`
	Checksum   string          `json:"checksum"`
	Usage      tenantUsageJSON `json:"usage"` // the usage counters (STORE-210)
	Quota      quotaJSON       `json:"quota"` // the effective quota (STORE-210)
}

type digestEntryJSON struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
	SHA256  string `json:"sha256"`
}

// digestDetailJSON is a tenant digest plus its per-key digests (?tenant=).
type digestDetailJSON struct {
	digestSummaryJSON
	Entries []digestEntryJSON `json:"entries"`
}

// digestRoute serves GET /v1/admin/digest, on primaries and replicas alike:
// per tenant, the key count, the total value bytes and a checksum over the
// sorted (key, version, SHA-256 of value) records, plus the seq the snapshot
// corresponds to (a primary's newest seq, or a replica's applied_seq). With
// ?tenant=<id> it returns that tenant only, with the per-key digests.
func (s *server) digestRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var tenantID int64
	if q := r.URL.Query(); q.Has("tenant") {
		n, err := strconv.ParseInt(q.Get("tenant"), 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "tenant must be a positive integer")
			return
		}
		tenantID = n
	}
	d, err := s.st.Digest(r.Context(), tenantID, tenantID != 0)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	tenants := make([]any, 0, len(d.Tenants))
	for _, t := range d.Tenants {
		sum := digestSummaryJSON{TenantID: t.TenantID, Name: t.Name, Keys: t.Keys, ValueBytes: t.ValueBytes, Checksum: t.Checksum,
			Usage: tenantUsageJSON{Keys: t.Usage.Keys, Bytes: t.Usage.Bytes}, Quota: toQuotaJSON(t.Quota)}
		if tenantID == 0 {
			tenants = append(tenants, sum)
			continue
		}
		detail := digestDetailJSON{digestSummaryJSON: sum, Entries: make([]digestEntryJSON, 0, len(t.Entries))}
		for _, e := range t.Entries {
			detail.Entries = append(detail.Entries, digestEntryJSON{Key: e.Key, Version: e.Version, SHA256: e.SHA256})
		}
		tenants = append(tenants, detail)
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": s.role, "seq": d.Seq, "tenants": tenants})
}
