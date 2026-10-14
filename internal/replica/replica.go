// Package replica keeps a read-only copy of a primary by pulling its admin
// change feed and applying each change in order.
package replica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"storecraft/internal/store"
)

const (
	defaultBatchLimit     = 100 // values can be up to 1 MiB each, so keep batches modest
	defaultMinBackoff     = 200 * time.Millisecond
	defaultMaxBackoff     = 10 * time.Second
	defaultRequestTimeout = 90 * time.Second // must exceed the primary's long-poll timeout (25s by default)
	minEmptyPollDuration  = 50 * time.Millisecond
	emptyPollPause        = 250 * time.Millisecond
)

// Config configures a Replica.
type Config struct {
	PrimaryURL string       // base URL of the primary, e.g. http://primary:8080
	Token      string       // the primary's admin token (STORECRAFT_REPLICATION_TOKEN)
	Store      *store.Store // this replica's database; must be a replica database
	Client     *http.Client // optional; defaults to one with a 90s timeout
	BatchLimit int          // changes per request; default 100
	MinBackoff time.Duration
	MaxBackoff time.Duration
	Log        *slog.Logger
}

// Replica pulls and applies the primary's changes.
type Replica struct {
	cfg        Config
	base       string
	primarySeq atomic.Int64
	lastErr    atomic.Pointer[string]
}

// New validates cfg and returns a Replica. Call Run to start replicating.
func New(cfg Config) (*Replica, error) {
	u, err := url.Parse(cfg.PrimaryURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("replica: primary URL %q must be an http(s) URL", cfg.PrimaryURL)
	}
	if cfg.Token == "" {
		return nil, errors.New("replica: a replication token is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("replica: a store is required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: defaultRequestTimeout}
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = defaultBatchLimit
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(defaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Replica{cfg: cfg, base: strings.TrimRight(cfg.PrimaryURL, "/")}, nil
}

// PrimarySeq is the primary's newest change seq as last reported by the
// primary (0 until the first successful pull).
func (r *Replica) PrimarySeq() int64 { return r.primarySeq.Load() }

// LastError is the most recent pull/apply error, or "" if the last attempt succeeded.
func (r *Replica) LastError() string {
	if p := r.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

func (r *Replica) setErr(err error) {
	if err == nil {
		r.lastErr.Store(nil)
		return
	}
	s := err.Error()
	r.lastErr.Store(&s)
}

// Run replicates until ctx is cancelled (then it returns nil). It resumes
// from the database's applied_seq, so it is safe to stop and start. Transient
// failures (primary unreachable, non-200, a bad batch) are retried with
// exponential backoff and jitter; only a database that is not a replica
// database is fatal.
func (r *Replica) Run(ctx context.Context) error {
	if _, err := r.cfg.Store.AppliedSeq(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // stopped during the first step: a normal stop, as in the loop below
		}
		return err
	}
	backoff := r.cfg.MinBackoff
	for ctx.Err() == nil {
		err := r.pullOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, store.ErrNotReplicaDB) {
			return err
		}
		if err != nil {
			r.setErr(err)
			r.cfg.Log.Warn("replication pull failed; will retry", "err", err, "retry_in", backoff)
			if !sleep(ctx, jitter(backoff)) {
				return nil
			}
			backoff = min(backoff*2, r.cfg.MaxBackoff)
			continue
		}
		r.setErr(nil)
		backoff = r.cfg.MinBackoff
	}
	return nil
}

// jitter returns a duration in [d/2, d].
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// sleep waits d or until ctx is done; it reports whether the full time passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type wireChange struct {
	Seq      int64           `json:"seq"`
	TenantID int64           `json:"tenant_id"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Op       string          `json:"op"`
	Version  int64           `json:"version"`
	Value    json.RawMessage `json:"value"`
	Subject  *string         `json:"subject"` // "quota" for a quota change (STORE-210)
	TS       string          `json:"ts"`
}

type wireFlag struct {
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
}

type wireResponse struct {
	Changes   []wireChange `json:"changes"`
	LastSeq   int64        `json:"last_seq"`
	LatestSeq int64        `json:"latest_seq"`
	// Flags is the primary's runtime flags (absent from a v1.0.0 primary).
	Flags *struct {
		QuotaEnforcement *wireFlag `json:"quota_enforcement"`
	} `json:"flags"`
}

// pullOnce fetches one batch after applied_seq and applies it in order. The
// primary long-polls, so an idle pull returns (empty) after its poll timeout.
func (r *Replica) pullOnce(ctx context.Context) error {
	applied, err := r.cfg.Store.AppliedSeq(ctx)
	if err != nil {
		return err
	}
	flag, err := r.cfg.Store.Flag(ctx, store.FlagQuotaEnforcement)
	if err != nil {
		return err
	}
	start := time.Now()
	// flags_version: the primary answers at once when our copy of the flag is
	// out of date, so a flag change arrives without waiting for its poll timeout.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/admin/changes?since=%d&limit=%d&flags_version=%d", r.base, applied, r.cfg.BatchLimit, flag.Version), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	res, err := r.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("contact primary: %w", urlErrorWithoutURL(err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("primary returned %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	var batch wireResponse
	if err := json.NewDecoder(res.Body).Decode(&batch); err != nil {
		return fmt.Errorf("decode primary response: %w", err)
	}
	if batch.LatestSeq > r.primarySeq.Load() {
		r.primarySeq.Store(batch.LatestSeq)
	}

	// Validate the whole batch before applying any of it: it must be exactly
	// applied+1, applied+2, ... (no gaps, no repeats).
	for i, c := range batch.Changes {
		if want := applied + 1 + int64(i); c.Seq != want {
			return fmt.Errorf("%w: primary sent seq %d where %d was expected", store.ErrReplicationGap, c.Seq, want)
		}
	}
	for _, c := range batch.Changes {
		ac, err := toAdminChange(c)
		if err != nil {
			return err
		}
		if err := r.cfg.Store.ApplyReplicated(ctx, ac); err != nil {
			return err
		}
	}
	if batch.Flags != nil && batch.Flags.QuotaEnforcement != nil {
		f := batch.Flags.QuotaEnforcement
		changed, err := r.cfg.Store.ApplyReplicatedFlag(ctx, store.Flag{Name: store.FlagQuotaEnforcement, Enabled: f.Enabled, Version: f.Version})
		if err != nil {
			return err
		}
		if changed {
			r.cfg.Log.Info("replicated flag change", "flag", store.FlagQuotaEnforcement, "enabled", f.Enabled, "version", f.Version)
		}
	}
	if len(batch.Changes) == 0 && time.Since(start) < minEmptyPollDuration {
		sleep(ctx, emptyPollPause) // an instantly-empty reply must not become a hot loop
	}
	return nil
}

func toAdminChange(c wireChange) (store.AdminChange, error) {
	ts, err := time.Parse(time.RFC3339Nano, c.TS)
	if err != nil {
		return store.AdminChange{}, fmt.Errorf("seq %d: bad timestamp %q: %w", c.Seq, c.TS, err)
	}
	ac := store.AdminChange{
		Change: store.Change{
			Seq: c.Seq, Key: c.Key, Op: c.Op, Version: c.Version, TS: ts,
			ValueSize: int64(len(c.Value)),
		},
		TenantID: c.TenantID,
		Kind:     c.Kind,
	}
	if c.Subject != nil {
		ac.Subject = *c.Subject
	}
	if c.Op == store.OpPut {
		ac.Value = []byte(c.Value) // keep a JSON null value as-is: it is a real value for a put
	} else {
		ac.ValueSize = 0
	}
	return ac, nil
}

// urlErrorWithoutURL drops the request URL from a transport error; the URL
// is known to the operator and the error is logged and shown in status.
func urlErrorWithoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
