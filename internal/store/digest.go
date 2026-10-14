package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeyDigest identifies one key's content: its version and the SHA-256 of its
// canonical value.
type KeyDigest struct {
	Key     string
	Version int64
	SHA256  string // hex
}

// TenantDigest summarizes one tenant's keys.
type TenantDigest struct {
	TenantID   int64
	Name       string
	Keys       int64
	ValueBytes int64  // total length of the canonical values
	Checksum   string // hex SHA-256 over the sorted (key, version, SHA-256 of value) records
	Entries    []KeyDigest
	Usage      Usage // the usage counters (tenant_usage), compared by verify
	Quota      Quota // the effective quota, compared by verify
}

// Digest is a consistent snapshot of every tenant's data together with the
// seq it corresponds to.
type Digest struct {
	// Seq is the newest change seq on a primary, or applied_seq (the PRIMARY's
	// seq of the last change applied) on a replica.
	Seq     int64
	Tenants []TenantDigest
}

// canonicalValue is the form values are hashed in: CanonicalJSON, the form
// they are stored in since STORE-210. Hashing it again keeps the digest
// stable for older databases whose values have not been backfilled yet. A
// value that is not valid JSON is used as stored.
func canonicalValue(v []byte) []byte { return CanonicalJSON(v) }

// tenantsWithKeys lists tenants plus any tenant id that has keys but no tenant
// row (possible on a legacy replica, which runs without foreign keys).
const tenantsWithKeys = `
SELECT id, name FROM tenants
UNION
SELECT DISTINCT tenant_id, '' FROM kv WHERE tenant_id NOT IN (SELECT id FROM tenants)`

// Digest summarizes the data. tenantID 0 means every tenant; otherwise only
// that tenant (ErrNotFound if it does not exist). withEntries also returns the
// per-key digests. The seq and the data come from one read transaction, so
// they always agree. It reads every value of the selected tenants.
func (s *Store) Digest(ctx context.Context, tenantID int64, withEntries bool) (Digest, error) {
	conn, done, err := s.snapshot(ctx)
	if err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	defer done()

	var d Digest
	err = conn.QueryRowContext(ctx, `SELECT applied_seq FROM replica_state WHERE id = 1`).Scan(&d.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM changes`).Scan(&d.Seq)
	}
	if err != nil {
		return Digest{}, fmt.Errorf("digest: seq: %w", err)
	}

	var tenants []TenantDigest
	query, args := tenantsWithKeys+` ORDER BY 1`, []any{}
	if tenantID != 0 {
		query, args = `SELECT id, name FROM (`+tenantsWithKeys+`) WHERE id = ?`, []any{tenantID}
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return Digest{}, fmt.Errorf("digest: tenants: %w", err)
	}
	for rows.Next() {
		var t TenantDigest
		if err := rows.Scan(&t.TenantID, &t.Name); err != nil {
			rows.Close()
			return Digest{}, fmt.Errorf("digest: tenants scan: %w", err)
		}
		tenants = append(tenants, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Digest{}, fmt.Errorf("digest: tenants rows: %w", err)
	}
	rows.Close()
	if tenantID != 0 && len(tenants) == 0 {
		return Digest{}, ErrNotFound
	}

	for i := range tenants {
		t := &tenants[i]
		if err := digestTenant(ctx, conn, t, withEntries); err != nil {
			return Digest{}, err
		}
		if t.Usage, err = readUsage(ctx, conn, t.TenantID); err != nil {
			return Digest{}, err
		}
		if t.Quota, err = readQuota(ctx, conn, t.TenantID); err != nil {
			return Digest{}, err
		}
	}
	d.Tenants = tenants
	return d, nil
}

func digestTenant(ctx context.Context, conn *sql.Conn, t *TenantDigest, withEntries bool) error {
	rows, err := conn.QueryContext(ctx, `SELECT key, version, value FROM kv WHERE tenant_id = ? ORDER BY key`, t.TenantID)
	if err != nil {
		return fmt.Errorf("digest: tenant %d: %w", t.TenantID, err)
	}
	defer rows.Close()

	h := sha256.New()
	var lenBuf [binary.MaxVarintLen64]byte
	var verBuf [8]byte
	for rows.Next() {
		var key string
		var version int64
		var value []byte
		if err := rows.Scan(&key, &version, &value); err != nil {
			return fmt.Errorf("digest: tenant %d scan: %w", t.TenantID, err)
		}
		canon := canonicalValue(value)
		sum := sha256.Sum256(canon)

		n := binary.PutUvarint(lenBuf[:], uint64(len(key)))
		h.Write(lenBuf[:n])
		h.Write([]byte(key))
		binary.BigEndian.PutUint64(verBuf[:], uint64(version))
		h.Write(verBuf[:])
		h.Write(sum[:])

		t.Keys++
		t.ValueBytes += int64(len(canon))
		if withEntries {
			t.Entries = append(t.Entries, KeyDigest{Key: key, Version: version, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("digest: tenant %d rows: %w", t.TenantID, err)
	}
	t.Checksum = hex.EncodeToString(h.Sum(nil))
	return nil
}
