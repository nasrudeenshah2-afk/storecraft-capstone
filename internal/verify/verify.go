// Package verify compares a primary and a replica through their admin digest
// endpoints and reports exactly where their data differs.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrTimeout means the comparison could not be completed in time: the replica
// did not reach the primary's seq, or no two digests at the same seq could be
// obtained (the primary kept moving). It is not a divergence.
var ErrTimeout = errors.New("verify: timed out")

const (
	defaultTimeout      = 30 * time.Second
	defaultPollInterval = 100 * time.Millisecond
)

// Config describes the two nodes to compare.
type Config struct {
	PrimaryURL   string
	ReplicaURL   string
	PrimaryToken string        // the primary's admin token
	ReplicaToken string        // the replica's admin token (often the same value)
	Timeout      time.Duration // whole run; default 30s
	PollInterval time.Duration // default 100ms
	Client       *http.Client
}

// Kinds of difference.
const (
	TenantMissingOnReplica = "tenant_missing_on_replica"
	TenantExtraOnReplica   = "tenant_extra_on_replica"
	TenantNameMismatch     = "tenant_name_mismatch"
	KeyMissingOnReplica    = "key_missing_on_replica"
	KeyExtraOnReplica      = "key_extra_on_replica"
	KeyMismatch            = "key_mismatch" // version and/or value hash differ
	TenantMismatch         = "tenant_mismatch"
	UsageMismatch          = "usage_mismatch" // the usage counters differ (STORE-210)
	QuotaMismatch          = "quota_mismatch" // the quota differs (STORE-210)
)

// KeyInfo is one side's view of a key.
type KeyInfo struct {
	Version int64
	SHA256  string
}

// Difference is one exact divergence.
type Difference struct {
	Kind     string
	TenantID int64
	Tenant   string
	Key      string   // empty for tenant-level differences
	Primary  *KeyInfo // set for key-level differences when the primary has the key
	Replica  *KeyInfo // set for key-level differences when the replica has the key
	Detail   string   // human-readable extra for tenant-level differences
}

// Result is the outcome of a completed comparison.
type Result struct {
	Equal       bool
	Seq         int64 // the seq both nodes were compared at
	Tenants     int   // as seen on the primary
	Keys        int64
	ValueBytes  int64
	Differences []Difference
}

// ExitCode maps an outcome to the process exit code: 0 equal, 1 diverged,
// 2 anything that is not a verdict (timeout, unreachable node, bad token...).
func ExitCode(res *Result, err error) int {
	switch {
	case err != nil || res == nil:
		return 2
	case !res.Equal:
		return 1
	}
	return 0
}

// ---- wire types ----

type keyDigest struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
	SHA256  string `json:"sha256"`
}

type usageDigest struct {
	Keys  int64 `json:"keys"`
	Bytes int64 `json:"bytes"`
}

type quotaDigest struct {
	MaxKeys  *int64 `json:"max_keys"`
	MaxBytes *int64 `json:"max_bytes"`
	Version  int64  `json:"version"`
}

func (q quotaDigest) String() string {
	lim := func(n *int64) string {
		if n == nil {
			return "unlimited"
		}
		return fmt.Sprint(*n)
	}
	return fmt.Sprintf("max_keys %s, max_bytes %s, version %d", lim(q.MaxKeys), lim(q.MaxBytes), q.Version)
}

type tenantDigest struct {
	TenantID   int64        `json:"tenant_id"`
	Name       string       `json:"name"`
	Keys       int64        `json:"keys"`
	ValueBytes int64        `json:"value_bytes"`
	Checksum   string       `json:"checksum"`
	Usage      *usageDigest `json:"usage"` // absent from nodes older than STORE-210
	Quota      *quotaDigest `json:"quota"` // absent from nodes older than STORE-210
	Entries    []keyDigest  `json:"entries"`
}

type digest struct {
	Role    string         `json:"role"`
	Seq     int64          `json:"seq"`
	Tenants []tenantDigest `json:"tenants"`
}

type node struct {
	name  string
	base  string
	token string
}

type verifier struct {
	cfg     Config
	primary node
	replica node
}

var errNotFound = errors.New("not found")

// Run compares the two nodes. It returns a Result when it reached a verdict
// (equal or diverged) and an error otherwise (wrapping ErrTimeout when time ran out).
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	for what, u := range map[string]string{"primary": cfg.PrimaryURL, "replica": cfg.ReplicaURL} {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return nil, fmt.Errorf("verify: %s URL %q must be an http(s) URL", what, u)
		}
	}
	if cfg.PrimaryToken == "" || cfg.ReplicaToken == "" {
		return nil, errors.New("verify: an admin token is required for both nodes")
	}
	v := &verifier{
		cfg:     cfg,
		primary: node{"primary", strings.TrimRight(cfg.PrimaryURL, "/"), cfg.PrimaryToken},
		replica: node{"replica", strings.TrimRight(cfg.ReplicaURL, "/"), cfg.ReplicaToken},
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	// interrupted: the caller stopped the run (Ctrl+C). That is not a timeout,
	// and saying "timed out ... replica has not reached" would be false.
	interrupted := func() error { return fmt.Errorf("verify: interrupted before a verdict: %w", parent.Err()) }
	why := "no comparison attempted yet"
	for {
		res, retryWhy, err := v.round(ctx)
		if err != nil {
			if parent.Err() != nil {
				return nil, interrupted()
			}
			if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
				return nil, fmt.Errorf("%w after %v: %s", ErrTimeout, cfg.Timeout, firstNonEmpty(timeoutDetail(err), why))
			}
			return nil, err
		}
		if res != nil {
			return res, nil
		}
		why = retryWhy
		if !sleep(ctx, cfg.PollInterval) {
			if parent.Err() != nil {
				return nil, interrupted()
			}
			return nil, fmt.Errorf("%w after %v: %s", ErrTimeout, cfg.Timeout, why)
		}
	}
}

func timeoutDetail(err error) string {
	if errors.Is(err, ErrTimeout) {
		return strings.TrimPrefix(err.Error(), ErrTimeout.Error()+": ")
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

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

// round makes one attempt. It returns a verdict, or (nil, reason, nil) when the
// attempt must be retried (the nodes were not at the same seq), or an error.
func (v *verifier) round(ctx context.Context) (*Result, string, error) {
	// 1. The primary's digest fixes the seq to compare at.
	pd, err := v.getDigest(ctx, v.primary, 0)
	if err != nil {
		return nil, "", err
	}
	if pd.Role != "primary" {
		return nil, "", fmt.Errorf("verify: %s is a %s, not a primary (check -primary and -replica)", v.primary.base, pd.Role)
	}

	// 2. Wait for the replica to apply everything up to that seq.
	if err := v.waitApplied(ctx, pd.Seq); err != nil {
		return nil, "", err
	}

	// 3. The replica's digest; it must be at exactly that seq to be comparable.
	rd, err := v.getDigest(ctx, v.replica, 0)
	if err != nil {
		return nil, "", err
	}
	if rd.Role != "replica" {
		return nil, "", fmt.Errorf("verify: %s is a %s, not a replica (check -primary and -replica)", v.replica.base, rd.Role)
	}
	if rd.Seq != pd.Seq {
		return nil, fmt.Sprintf("primary moved on (digests at seq %d vs %d); no same-seq comparison yet", pd.Seq, rd.Seq), nil
	}

	// 4. Compare tenant by tenant.
	diffs, detailTenants := compareSummaries(pd, rd)
	for _, id := range detailTenants {
		pdt, perr := v.getDigest(ctx, v.primary, id)
		rdt, rerr := v.getDigest(ctx, v.replica, id)
		if perr != nil || rerr != nil {
			if errors.Is(perr, errNotFound) || errors.Is(rerr, errNotFound) {
				return nil, "a tenant disappeared while comparing", nil
			}
			return nil, "", errors.Join(perr, rerr)
		}
		if pdt.Seq != pd.Seq || rdt.Seq != pd.Seq {
			return nil, fmt.Sprintf("seq moved while fetching key digests (%d, %d, %d)", pd.Seq, pdt.Seq, rdt.Seq), nil
		}
		diffs = append(diffs, compareKeys(pdt.Tenants[0], rdt.Tenants[0])...)
	}

	res := &Result{Seq: pd.Seq, Tenants: len(pd.Tenants)}
	for _, t := range pd.Tenants {
		res.Keys += t.Keys
		res.ValueBytes += t.ValueBytes
	}
	sort.SliceStable(diffs, func(i, j int) bool {
		if diffs[i].TenantID != diffs[j].TenantID {
			return diffs[i].TenantID < diffs[j].TenantID
		}
		return diffs[i].Key < diffs[j].Key
	})
	res.Differences = diffs
	res.Equal = len(diffs) == 0
	return res, "", nil
}

// compareSummaries compares the per-tenant summaries. It returns tenant-level
// differences and the ids of tenants present on both sides that need a
// key-by-key comparison.
func compareSummaries(pd, rd digest) (diffs []Difference, detail []int64) {
	rep := map[int64]tenantDigest{}
	for _, t := range rd.Tenants {
		rep[t.TenantID] = t
	}
	seen := map[int64]bool{}
	for _, p := range pd.Tenants {
		seen[p.TenantID] = true
		r, ok := rep[p.TenantID]
		if !ok {
			diffs = append(diffs, Difference{Kind: TenantMissingOnReplica, TenantID: p.TenantID, Tenant: p.Name,
				Detail: fmt.Sprintf("primary has %d keys", p.Keys)})
			continue
		}
		if p.Name != r.Name {
			diffs = append(diffs, Difference{Kind: TenantNameMismatch, TenantID: p.TenantID, Tenant: p.Name,
				Detail: fmt.Sprintf("primary name %q, replica name %q", p.Name, r.Name)})
		}
		if p.Keys != r.Keys || p.ValueBytes != r.ValueBytes || p.Checksum != r.Checksum {
			detail = append(detail, p.TenantID)
		}
		// Usage and quota (STORE-210): compared only when both nodes report them.
		if p.Usage != nil && r.Usage != nil && *p.Usage != *r.Usage {
			diffs = append(diffs, Difference{Kind: UsageMismatch, TenantID: p.TenantID, Tenant: p.Name,
				Detail: fmt.Sprintf("primary %d keys / %d bytes, replica %d keys / %d bytes", p.Usage.Keys, p.Usage.Bytes, r.Usage.Keys, r.Usage.Bytes)})
		}
		if p.Quota != nil && r.Quota != nil && p.Quota.String() != r.Quota.String() {
			diffs = append(diffs, Difference{Kind: QuotaMismatch, TenantID: p.TenantID, Tenant: p.Name,
				Detail: fmt.Sprintf("primary %s; replica %s", p.Quota, r.Quota)})
		}
	}
	for _, r := range rd.Tenants {
		if !seen[r.TenantID] {
			diffs = append(diffs, Difference{Kind: TenantExtraOnReplica, TenantID: r.TenantID, Tenant: r.Name,
				Detail: fmt.Sprintf("replica has %d keys", r.Keys)})
		}
	}
	return diffs, detail
}

// compareKeys diffs two tenants' per-key digests (both sorted by key).
func compareKeys(p, r tenantDigest) []Difference {
	var diffs []Difference
	rk := map[string]keyDigest{}
	for _, e := range r.Entries {
		rk[e.Key] = e
	}
	seen := map[string]bool{}
	base := Difference{TenantID: p.TenantID, Tenant: p.Name}
	for _, pe := range p.Entries {
		seen[pe.Key] = true
		re, ok := rk[pe.Key]
		d := base
		d.Key = pe.Key
		pi := &KeyInfo{pe.Version, pe.SHA256}
		switch {
		case !ok:
			d.Kind, d.Primary = KeyMissingOnReplica, pi
		case re.Version != pe.Version || re.SHA256 != pe.SHA256:
			d.Kind, d.Primary, d.Replica = KeyMismatch, pi, &KeyInfo{re.Version, re.SHA256}
		default:
			continue
		}
		diffs = append(diffs, d)
	}
	for _, re := range r.Entries {
		if !seen[re.Key] {
			d := base
			d.Key, d.Kind, d.Replica = re.Key, KeyExtraOnReplica, &KeyInfo{re.Version, re.SHA256}
			diffs = append(diffs, d)
		}
	}
	if len(diffs) == 0 {
		// The summaries differed but every key matches: never hide that.
		d := base
		d.Kind = TenantMismatch
		d.Detail = fmt.Sprintf("summaries differ (primary %d keys/%d bytes, replica %d keys/%d bytes) but no key differs", p.Keys, p.ValueBytes, r.Keys, r.ValueBytes)
		diffs = append(diffs, d)
	}
	return diffs
}

// waitApplied polls the replica's status until applied_seq >= seq.
func (v *verifier) waitApplied(ctx context.Context, seq int64) error {
	var last int64 = -1
	for {
		var st struct {
			Role       string `json:"role"`
			AppliedSeq int64  `json:"applied_seq"`
		}
		if err := v.getJSON(ctx, v.replica, "/v1/replication/status", &st); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: replica applied_seq %d has not reached the primary's seq %d", ErrTimeout, max(last, 0), seq)
			}
			return err
		}
		if st.Role != "replica" {
			return fmt.Errorf("verify: %s is a %s, not a replica (check -primary and -replica)", v.replica.base, st.Role)
		}
		last = st.AppliedSeq
		if st.AppliedSeq >= seq {
			return nil
		}
		if !sleep(ctx, v.cfg.PollInterval) {
			return fmt.Errorf("%w: replica applied_seq %d has not reached the primary's seq %d", ErrTimeout, last, seq)
		}
	}
}

func (v *verifier) getDigest(ctx context.Context, n node, tenantID int64) (digest, error) {
	path := "/v1/admin/digest"
	if tenantID != 0 {
		path += fmt.Sprintf("?tenant=%d", tenantID)
	}
	var d digest
	if err := v.getJSON(ctx, n, path, &d); err != nil {
		return digest{}, err
	}
	if tenantID != 0 && len(d.Tenants) != 1 {
		return digest{}, fmt.Errorf("verify: %s returned %d tenants for a single-tenant digest", n.name, len(d.Tenants))
	}
	return d, nil
}

// getJSON performs an authenticated GET. Errors never include the token.
func (v *verifier) getJSON(ctx context.Context, n node, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	res, err := v.cfg.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("verify: cannot reach the %s at %s: %w", n.name, n.base, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && strings.Contains(path, "tenant=") {
		return errNotFound
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("verify: the %s answered %s for %s: %s", n.name, res.Status, strings.SplitN(path, "?", 2)[0], strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("verify: bad response from the %s: %w", n.name, err)
	}
	return nil
}

// Format writes the human-readable verdict.
func (r *Result) Format(w io.Writer) {
	if r.Equal {
		fmt.Fprintf(w, "OK: zero divergence (primary and replica both at seq %d: %d tenants, %d keys, %d value bytes)\n",
			r.Seq, r.Tenants, r.Keys, r.ValueBytes)
		return
	}
	fmt.Fprintf(w, "DIVERGED: %d difference(s) with both nodes at seq %d\n", len(r.Differences), r.Seq)
	for _, d := range r.Differences {
		who := fmt.Sprintf("tenant %s (id %d)", d.Tenant, d.TenantID)
		if d.Tenant == "" {
			who = fmt.Sprintf("tenant (id %d, no tenant record)", d.TenantID)
		}
		switch d.Kind {
		case KeyMismatch:
			fmt.Fprintf(w, "  %s key %q: primary version %d sha256 %s / replica version %d sha256 %s\n",
				who, d.Key, d.Primary.Version, d.Primary.SHA256, d.Replica.Version, d.Replica.SHA256)
		case KeyMissingOnReplica:
			fmt.Fprintf(w, "  %s key %q: missing on replica (primary version %d sha256 %s)\n",
				who, d.Key, d.Primary.Version, d.Primary.SHA256)
		case KeyExtraOnReplica:
			fmt.Fprintf(w, "  %s key %q: extra on replica (replica version %d sha256 %s)\n",
				who, d.Key, d.Replica.Version, d.Replica.SHA256)
		case TenantMissingOnReplica:
			fmt.Fprintf(w, "  %s: missing on replica (%s)\n", who, d.Detail)
		case TenantExtraOnReplica:
			fmt.Fprintf(w, "  %s: extra on replica (%s)\n", who, d.Detail)
		default:
			fmt.Fprintf(w, "  %s: %s (%s)\n", who, d.Kind, d.Detail)
		}
	}
}
