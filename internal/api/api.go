// Package api is the storecraft HTTP layer.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/feed"
	"storecraft/internal/metrics"
	"storecraft/internal/store"
)

const maxBodyBytes = 1 << 20 // 1 MiB

const (
	defaultPollTimeout  = 25 * time.Second
	defaultSSEHeartbeat = 15 * time.Second
	defaultChangeLimit  = 100
	maxChangeLimit      = 500
)

// Options configures the handler.
type Options struct {
	AdminToken  string        // empty disables admin access
	Log         *slog.Logger  // defaults to slog.Default()
	PollTimeout time.Duration // long-poll hold time for /v1/changes; defaults to 25s

	SSEHeartbeat time.Duration // idle interval between SSE heartbeat comments; defaults to 15s

	Role    string            // RolePrimary (default) or RoleReplica; a replica rejects writes with 409
	Replica ReplicationSource // replica only: reports the primary's newest seq (for status/lag)
}

type server struct {
	st          *store.Store
	feed        *feed.Feed
	adminHash   string // empty when admin access is disabled
	log         *slog.Logger
	pollTimeout time.Duration
	heartbeat   time.Duration
	m           *metrics.Metrics
	role        string
	readOnly    bool // true on a replica
	replica     ReplicationSource
}

// New returns the HTTP handler.
func New(st *store.Store, opts Options) http.Handler {
	s := &server{st: st, feed: feed.New(st), m: metrics.New(), log: opts.Log, pollTimeout: opts.PollTimeout, heartbeat: opts.SSEHeartbeat}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.heartbeat <= 0 {
		s.heartbeat = defaultSSEHeartbeat
	}
	switch opts.Role {
	case "", RolePrimary:
		s.role = RolePrimary
	case RoleReplica:
		s.role, s.readOnly, s.replica = RoleReplica, true, opts.Replica
	default:
		panic("api: unknown role " + opts.Role)
	}
	if s.pollTimeout <= 0 {
		s.pollTimeout = defaultPollTimeout
	}
	if opts.AdminToken != "" {
		s.adminHash = auth.HashToken(opts.AdminToken)
	}
	s.m.AddQuotaGauges(s.scrapeUsage, s.scrapeEnforcement)
	return s.recoverer(s.mux())
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "panic", v, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
}

// authenticate resolves the bearer token to a tenant. It writes the error
// response and returns ok=false on failure (including admin tokens, which
// carry no tenant and so are forbidden from tenant data routes).
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (store.Tenant, bool) {
	unauthorized := func() (store.Tenant, bool) {
		writeUnauthorized(w)
		return store.Tenant{}, false
	}
	token, ok := bearerToken(r)
	if !ok {
		return unauthorized()
	}
	hash := auth.HashToken(token)
	if s.isAdminHash(hash) {
		writeError(w, http.StatusForbidden, codeForbidden, "admin token cannot access tenant data")
		return store.Tenant{}, false
	}
	t, err := s.st.TenantByTokenHash(r.Context(), hash)
	if errors.Is(err, store.ErrNotFound) {
		return unauthorized()
	}
	if err != nil {
		s.internal(w, r, err)
		return store.Tenant{}, false
	}
	return t, true
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

type entryJSON struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version"`
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid bearer token")
}

// isAdminHash reports whether hash is the admin token's hash (never true
// when admin access is disabled).
func (s *server) isAdminHash(hash string) bool {
	return s.adminHash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(s.adminHash)) == 1
}

// withKey adapts a key handler: it resolves {key}, authenticates the tenant
// and then calls fn. An empty key (e.g. "/v1/keys/") is a plain 404.
func (s *server) withKey(fn func(w http.ResponseWriter, r *http.Request, t store.Tenant, key string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "no such route")
			return
		}
		tenant, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		fn(w, r, tenant, key)
	}
}

func (s *server) get(w http.ResponseWriter, r *http.Request, t store.Tenant, key string) {
	e, err := s.st.Get(r.Context(), t.ID, key)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "key not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(e.Version))
	writeJSON(w, http.StatusOK, entryJSON{Key: e.Key, Value: e.Value, Version: e.Version})
}

func (s *server) listRoute(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	entries, seq, err := s.st.ListAt(r.Context(), tenant.ID, r.URL.Query().Get("prefix"))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{Key: e.Key, Value: e.Value, Version: e.Version})
	}
	// seq: where to start following the change feed so nothing after this list is missed.
	writeJSON(w, http.StatusOK, map[string]any{"keys": out, "seq": seq})
}
