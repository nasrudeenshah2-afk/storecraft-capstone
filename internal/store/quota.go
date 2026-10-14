package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// STORE-210: per-tenant quotas, usage and the quota_enforcement flag.

// Default quota of a tenant that has no quota record (spec section 2).
const (
	DefaultMaxKeys  int64 = 1000
	DefaultMaxBytes int64 = 1048576
)

// FlagQuotaEnforcement is the runtime flag that turns quota rejections on.
const FlagQuotaEnforcement = "quota_enforcement"

// SubjectQuota marks a ChangeTenant change row that carries a quota change.
const SubjectQuota = "quota"

// Quota dimensions, as reported in QuotaError and the rejection metric.
const (
	DimensionKeys  = "keys"
	DimensionBytes = "bytes"
)

// Quota is a tenant's limits. A nil limit means unlimited. Version is 0 for
// the default quota (no record) and goes up by one with every edit.
type Quota struct {
	MaxKeys  *int64
	MaxBytes *int64
	Version  int64
}

// Usage is what a tenant stores: its key count and total value bytes.
type Usage struct {
	Keys  int64
	Bytes int64
}

// TenantSummary is a tenant with its quota and usage (admin views).
type TenantSummary struct {
	ID    int64
	Name  string
	Quota Quota
	Usage Usage
}

// UsageReport is a tenant's usage and quota read together, with the change
// seq they correspond to.
type UsageReport struct {
	TenantID int64
	Usage    Usage
	Quota    Quota
	Seq      int64
}

// Flag is a runtime feature flag. Version is 0 until it is first changed.
type Flag struct {
	Name    string
	Enabled bool
	Version int64
}

// QuotaError is returned by a write that would exceed a quota while
// enforcement is on. Nothing was written.
type QuotaError struct {
	Dimension string // DimensionKeys or DimensionBytes
	Used      int64
	Limit     int64
	Requested int64 // 1 key, or the number of bytes the write would add
}

func (e *QuotaError) Error() string {
	if e.Dimension == DimensionKeys {
		return fmt.Sprintf("This write would add a key, but %d of %d keys are used.", e.Used, e.Limit)
	}
	unit := "bytes"
	if e.Requested == 1 {
		unit = "byte"
	}
	return fmt.Sprintf("This write needs %d more %s, but %d of %d bytes are used.", e.Requested, unit, e.Used, e.Limit)
}

// queryRower is what the read helpers need: *sql.DB, *sql.Tx and *sql.Conn.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// quotaRecord is the JSON value of a SubjectQuota change row.
type quotaRecord struct {
	MaxKeys  *int64 `json:"max_keys"`
	MaxBytes *int64 `json:"max_bytes"`
	Version  int64  `json:"version"`
}

func defaultQuota() Quota {
	k, b := DefaultMaxKeys, DefaultMaxBytes
	return Quota{MaxKeys: &k, MaxBytes: &b}
}

func nullable(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// readQuota returns the tenant's quota (the default quota if it has no record).
func readQuota(ctx context.Context, q queryRower, tenantID int64) (Quota, error) {
	var mk, mb sql.NullInt64
	var version int64
	err := q.QueryRowContext(ctx, `SELECT max_keys, max_bytes, version FROM tenant_quotas WHERE tenant_id = ?`, tenantID).
		Scan(&mk, &mb, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultQuota(), nil
	}
	if err != nil {
		return Quota{}, fmt.Errorf("read quota of tenant %d: %w", tenantID, err)
	}
	return Quota{MaxKeys: nullable(mk), MaxBytes: nullable(mb), Version: version}, nil
}

// readUsage returns the tenant's usage counters (zero if it has none).
func readUsage(ctx context.Context, q queryRower, tenantID int64) (Usage, error) {
	var u Usage
	err := q.QueryRowContext(ctx, `SELECT key_count, byte_count FROM tenant_usage WHERE tenant_id = ?`, tenantID).Scan(&u.Keys, &u.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("read usage of tenant %d: %w", tenantID, err)
	}
	return u, nil
}

// readFlag returns a flag (off at version 0 if it was never changed).
func readFlag(ctx context.Context, q queryRower, name string) (Flag, error) {
	f := Flag{Name: name}
	var enabled int
	err := q.QueryRowContext(ctx, `SELECT enabled, version FROM flags WHERE name = ?`, name).Scan(&enabled, &f.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return Flag{}, fmt.Errorf("read flag %s: %w", name, err)
	}
	f.Enabled = enabled == 1
	return f, nil
}

// checkQuota decides whether a write that adds addKeys keys and addBytes bytes
// may proceed. It runs INSIDE the write transaction, after the version
// preconditions, so the flag, the quota and the usage it reads are exactly
// what the write will change (no check-then-write race: every writer holds
// the same write lock). Writes that add no key and no bytes always pass.
func checkQuota(ctx context.Context, tx *sql.Tx, tenantID, addKeys, addBytes int64) error {
	if addKeys <= 0 && addBytes <= 0 {
		return nil
	}
	f, err := readFlag(ctx, tx, FlagQuotaEnforcement)
	if err != nil || !f.Enabled {
		return err
	}
	q, err := readQuota(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	u, err := readUsage(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if addKeys > 0 && q.MaxKeys != nil && u.Keys+addKeys > *q.MaxKeys {
		return &QuotaError{Dimension: DimensionKeys, Used: u.Keys, Limit: *q.MaxKeys, Requested: addKeys}
	}
	if addBytes > 0 && q.MaxBytes != nil && u.Bytes+addBytes > *q.MaxBytes {
		return &QuotaError{Dimension: DimensionBytes, Used: u.Bytes, Limit: *q.MaxBytes, Requested: addBytes}
	}
	return nil
}

// Quota returns a tenant's quota; ErrNotFound if the tenant does not exist.
func (s *Store) Quota(ctx context.Context, tenantID int64) (Quota, error) {
	conn, done, err := s.snapshot(ctx)
	if err != nil {
		return Quota{}, fmt.Errorf("quota: %w", err)
	}
	defer done()
	if err := tenantExists(ctx, conn, tenantID); err != nil {
		return Quota{}, err
	}
	return readQuota(ctx, conn, tenantID)
}

func tenantExists(ctx context.Context, q queryRower, tenantID int64) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM tenants WHERE id = ?`, tenantID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("tenant %d: %w", tenantID, err)
	}
	return nil
}

// SetQuota replaces a tenant's quota if its current version equals expected
// (compare-and-swap; 0 = never edited). It returns the previous and the new
// quota. ErrNotFound: no such tenant; ErrConflict: stale version. The change
// is logged as a ChangeTenant row with subject "quota" in the same
// transaction, so replicas receive it in order.
func (s *Store) SetQuota(ctx context.Context, tenantID int64, maxKeys, maxBytes *int64, expected int64) (prev, next Quota, err error) {
	err = s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM tenants WHERE id = ?`, tenantID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, ErrNotFound
		}
		if err != nil {
			return 0, false, fmt.Errorf("set quota: %w", err)
		}
		if prev, err = readQuota(ctx, tx, tenantID); err != nil {
			return 0, false, err
		}
		if prev.Version != expected {
			return 0, false, ErrConflict
		}
		next = Quota{MaxKeys: maxKeys, MaxBytes: maxBytes, Version: expected + 1}
		now := time.Now().UnixNano()
		if err := upsertQuota(ctx, tx, tenantID, next, now); err != nil {
			return 0, false, err
		}
		rec, err := json.Marshal(quotaRecord{MaxKeys: next.MaxKeys, MaxBytes: next.MaxBytes, Version: next.Version})
		if err != nil {
			return 0, false, err
		}
		if err := recordChange(ctx, tx, tenantID, ChangeTenant, name, OpPut, next.Version, rec, now, SubjectQuota); err != nil {
			return 0, false, err
		}
		return tenantID, true, nil
	})
	return prev, next, err
}

func upsertQuota(ctx context.Context, tx *sql.Tx, tenantID int64, q Quota, ts int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO tenant_quotas(tenant_id, max_keys, max_bytes, version, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(tenant_id) DO UPDATE SET
	max_keys = excluded.max_keys, max_bytes = excluded.max_bytes,
	version = excluded.version, updated_at = excluded.updated_at`,
		tenantID, q.MaxKeys, q.MaxBytes, q.Version, ts)
	if err != nil {
		return fmt.Errorf("store quota of tenant %d: %w", tenantID, err)
	}
	return nil
}

// UsageAt returns a tenant's usage and quota and this database's newest change
// seq, all from one snapshot.
func (s *Store) UsageAt(ctx context.Context, tenantID int64) (UsageReport, error) {
	conn, done, err := s.snapshot(ctx)
	if err != nil {
		return UsageReport{}, fmt.Errorf("usage: %w", err)
	}
	defer done()
	r := UsageReport{TenantID: tenantID}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM changes`).Scan(&r.Seq); err != nil {
		return UsageReport{}, fmt.Errorf("usage: seq: %w", err)
	}
	if r.Usage, err = readUsage(ctx, conn, tenantID); err != nil {
		return UsageReport{}, err
	}
	if r.Quota, err = readQuota(ctx, conn, tenantID); err != nil {
		return UsageReport{}, err
	}
	return r, nil
}

const tenantSummaries = `
SELECT t.id, t.name, q.max_keys, q.max_bytes, q.version, COALESCE(u.key_count, 0), COALESCE(u.byte_count, 0)
FROM tenants t
LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
LEFT JOIN tenant_usage u ON u.tenant_id = t.id`

// Tenants lists every tenant with its quota and usage, sorted by id.
func (s *Store) Tenants(ctx context.Context) ([]TenantSummary, error) {
	return s.tenantSummaries(ctx, tenantSummaries+` ORDER BY t.id`)
}

// TenantSummary returns one tenant with its quota and usage (ErrNotFound if
// it does not exist).
func (s *Store) TenantSummary(ctx context.Context, tenantID int64) (TenantSummary, error) {
	out, err := s.tenantSummaries(ctx, tenantSummaries+` WHERE t.id = ?`, tenantID)
	if err != nil {
		return TenantSummary{}, err
	}
	if len(out) == 0 {
		return TenantSummary{}, ErrNotFound
	}
	return out[0], nil
}

func (s *Store) tenantSummaries(ctx context.Context, query string, args ...any) ([]TenantSummary, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	defer rows.Close()
	var out []TenantSummary
	for rows.Next() {
		var t TenantSummary
		var mk, mb, version sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &mk, &mb, &version, &t.Usage.Keys, &t.Usage.Bytes); err != nil {
			return nil, fmt.Errorf("tenants scan: %w", err)
		}
		if version.Valid {
			t.Quota = Quota{MaxKeys: nullable(mk), MaxBytes: nullable(mb), Version: version.Int64}
		} else {
			t.Quota = defaultQuota()
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TenantUsage is one tenant's usage, for the per-tenant usage metrics.
type TenantUsage struct {
	TenantID int64
	Usage    Usage
}

// AllUsage returns every tenant's usage counters, sorted by tenant id.
func (s *Store) AllUsage(ctx context.Context) ([]TenantUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, COALESCE(u.key_count, 0), COALESCE(u.byte_count, 0)
FROM tenants t LEFT JOIN tenant_usage u ON u.tenant_id = t.id ORDER BY t.id`)
	if err != nil {
		return nil, fmt.Errorf("all usage: %w", err)
	}
	defer rows.Close()
	var out []TenantUsage
	for rows.Next() {
		var tu TenantUsage
		if err := rows.Scan(&tu.TenantID, &tu.Usage.Keys, &tu.Usage.Bytes); err != nil {
			return nil, fmt.Errorf("all usage scan: %w", err)
		}
		out = append(out, tu)
	}
	return out, rows.Err()
}

// Flag returns a runtime flag (off at version 0 if never changed). This is the
// one place every reader of a flag goes through; it is read on every use, so
// a change takes effect without a restart.
func (s *Store) Flag(ctx context.Context, name string) (Flag, error) {
	return readFlag(ctx, s.db, name)
}

// SetFlag changes a flag if its current version equals expected
// (compare-and-swap). It returns the previous and the new state; ErrConflict
// for a stale version. Long-pollers of the admin feed are woken so replicas
// learn the new state at once.
func (s *Store) SetFlag(ctx context.Context, name string, enabled bool, expected int64) (prev, next Flag, err error) {
	err = s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		if prev, err = readFlag(ctx, tx, name); err != nil {
			return 0, false, err
		}
		if prev.Version != expected {
			return 0, false, ErrConflict
		}
		next = Flag{Name: name, Enabled: enabled, Version: expected + 1}
		if err := upsertFlag(ctx, tx, next); err != nil {
			return 0, false, err
		}
		return 0, true, nil
	})
	return prev, next, err
}

func upsertFlag(ctx context.Context, tx *sql.Tx, f Flag) error {
	enabled := 0
	if f.Enabled {
		enabled = 1
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO flags(name, enabled, version, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET enabled = excluded.enabled, version = excluded.version, updated_at = excluded.updated_at`,
		f.Name, enabled, f.Version, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("store flag %s: %w", f.Name, err)
	}
	return nil
}

// ApplyReplicatedFlag stores a flag state received from the primary whenever
// its version differs from the one held here. The primary is the authority:
// a replica holding a HIGHER version (for example after the primary was
// restored from a backup) takes the primary's state too, so it converges
// instead of polling in a loop (review finding R6). The feed is pulled one
// request at a time, so an older state never arrives after a newer one.
// It reports whether it changed.
func (s *Store) ApplyReplicatedFlag(ctx context.Context, f Flag) (bool, error) {
	changed := false
	err := s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		cur, err := readFlag(ctx, tx, f.Name)
		if err != nil {
			return 0, false, err
		}
		if f.Version == cur.Version {
			return 0, false, nil
		}
		changed = true
		if f.Version == 0 { // never changed on the primary: no row means off at version 0
			_, err := tx.ExecContext(ctx, `DELETE FROM flags WHERE name = ?`, f.Name)
			return 0, false, err
		}
		return 0, false, upsertFlag(ctx, tx, f)
	})
	return changed, err
}
