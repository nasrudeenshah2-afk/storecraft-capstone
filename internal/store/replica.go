package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrNotReplicaDB means the database has no replica_state row: it is not a
	// replica database.
	ErrNotReplicaDB = errors.New("store: not a replica database")
	// ErrReplicationGap means a replicated change was not the next one in the
	// primary's sequence, so applying it would leave a gap.
	ErrReplicationGap = errors.New("store: replication gap")
)

// IsReplica reports whether this database is a replica database.
func (s *Store) IsReplica(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replica_state`).Scan(&n); err != nil {
		return false, fmt.Errorf("replica state: %w", err)
	}
	return n > 0, nil
}

// EnsureReplica marks the database as a replica (applied_seq 0) if it is not
// one yet. It refuses (ErrNotReplicaDB) when the database already holds
// tenants or changes of its own, so a primary can never be turned into a
// replica by accident.
func (s *Store) EnsureReplica(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM replica_state`).Scan(&n); err != nil {
			return 0, false, fmt.Errorf("replica state: %w", err)
		}
		if n > 0 {
			return 0, false, nil
		}
		var data int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tenants) + (SELECT COUNT(*) FROM changes)`).Scan(&data); err != nil {
			return 0, false, fmt.Errorf("check database is empty: %w", err)
		}
		if data > 0 {
			return 0, false, fmt.Errorf("%w: it already has tenants or changes of its own", ErrNotReplicaDB)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO replica_state(id, applied_seq) VALUES (1, 0)`); err != nil {
			return 0, false, fmt.Errorf("init replica state: %w", err)
		}
		return 0, false, nil
	})
}

// AppliedSeq returns the primary's seq of the last change applied here, or
// ErrNotReplicaDB if this is not a replica database.
func (s *Store) AppliedSeq(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT applied_seq FROM replica_state WHERE id = 1`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotReplicaDB
	}
	if err != nil {
		return 0, fmt.Errorf("applied seq: %w", err)
	}
	return n, nil
}

// ApplyReplicated applies one change from the primary's log (c.Seq is the
// PRIMARY's seq) in a single transaction that also records applied_seq:
//
//   - c.Seq <= applied_seq: already applied, nothing happens (idempotent).
//   - c.Seq == applied_seq+1: applied with the primary's version, then
//     applied_seq advances. The replica also writes its own change row (with
//     its own local seq, never the primary's) so long-poll and SSE work here.
//   - anything else: ErrReplicationGap, nothing is applied.
//
// Data operations are themselves idempotent (explicit versions, upserts, a
// delete of a missing key is fine), and a change that had no effect writes no
// local change row.
func (s *Store) ApplyReplicated(ctx context.Context, c AdminChange) error {
	return s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		var applied int64
		err := tx.QueryRowContext(ctx, `SELECT applied_seq FROM replica_state WHERE id = 1`).Scan(&applied)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, ErrNotReplicaDB
		}
		if err != nil {
			return 0, false, fmt.Errorf("read applied seq: %w", err)
		}
		if c.Seq <= applied {
			return 0, false, nil
		}
		if c.Seq != applied+1 {
			return 0, false, fmt.Errorf("%w: applied %d, got %d", ErrReplicationGap, applied, c.Seq)
		}

		ts := c.TS.UnixNano()
		changed := false
		switch c.Kind {
		case ChangeTenant:
			if c.Subject == SubjectQuota {
				var rec quotaRecord
				if err := json.Unmarshal(c.Value, &rec); err != nil {
					return 0, false, fmt.Errorf("seq %d: bad quota record: %w", c.Seq, err)
				}
				if err := upsertQuota(ctx, tx, c.TenantID, Quota{MaxKeys: rec.MaxKeys, MaxBytes: rec.MaxBytes, Version: rec.Version}, ts); err != nil {
					return 0, false, fmt.Errorf("seq %d: %w", c.Seq, err)
				}
				changed = true
				if err := recordChange(ctx, tx, c.TenantID, ChangeTenant, c.Key, OpPut, rec.Version, c.Value, ts, SubjectQuota); err != nil {
					return 0, false, err
				}
				break
			}
			var rec tenantRecord
			if err := json.Unmarshal(c.Value, &rec); err != nil {
				return 0, false, fmt.Errorf("seq %d: bad tenant record: %w", c.Seq, err)
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO tenants(id, name, token_hash) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
				c.TenantID, rec.Name, rec.TokenHash)
			if err != nil {
				return 0, false, fmt.Errorf("seq %d: apply tenant: %w", c.Seq, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				changed = true
				if err := recordChange(ctx, tx, c.TenantID, ChangeTenant, rec.Name, OpPut, 1, c.Value, ts, ""); err != nil {
					return 0, false, err
				}
			}
		case ChangeKey:
			switch c.Op {
			case OpPut:
				// Canonical bytes, whatever encoding the feed used (a v1.0.0
				// primary sends Go's HTML-escaped form).
				value := CanonicalJSON(c.Value)
				if _, err := tx.ExecContext(ctx, `
INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(tenant_id, key) DO UPDATE SET
	value = excluded.value,
	version = excluded.version,
	updated_at = excluded.updated_at`, c.TenantID, c.Key, value, c.Version, ts); err != nil {
					return 0, false, fmt.Errorf("seq %d: apply put %q: %w", c.Seq, c.Key, err)
				}
				changed = true
				if err := recordChange(ctx, tx, c.TenantID, ChangeKey, c.Key, OpPut, c.Version, value, ts, ""); err != nil {
					return 0, false, err
				}
			case OpDelete:
				res, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE tenant_id = ? AND key = ?`, c.TenantID, c.Key)
				if err != nil {
					return 0, false, fmt.Errorf("seq %d: apply delete %q: %w", c.Seq, c.Key, err)
				}
				if n, _ := res.RowsAffected(); n > 0 {
					changed = true
					if err := recordChange(ctx, tx, c.TenantID, ChangeKey, c.Key, OpDelete, c.Version, nil, ts, ""); err != nil {
						return 0, false, err
					}
				}
			default:
				return 0, false, fmt.Errorf("seq %d: unknown op %q", c.Seq, c.Op)
			}
		default:
			return 0, false, fmt.Errorf("seq %d: unknown kind %q", c.Seq, c.Kind)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE replica_state SET applied_seq = ? WHERE id = 1`, c.Seq); err != nil {
			return 0, false, fmt.Errorf("record applied seq: %w", err)
		}
		return c.TenantID, changed, nil
	})
}
