package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// snapshot opens a read transaction on a dedicated connection: every query on
// the returned conn sees the same consistent state of the database (WAL
// snapshot isolation), even while writers commit. Call done when finished.
//
// It uses an explicit BEGIN because db.BeginTx would apply _txlock=immediate
// from the DSN and take the write lock.
func (s *Store) snapshot(ctx context.Context) (conn *sql.Conn, done func(), err error) {
	conn, err = s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("begin: %w", err)
	}
	return conn, func() {
		conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		conn.Close()
	}, nil
}

// ListAt is List plus the seq the list corresponds to: this database's newest
// change seq, read in the same snapshot as the entries. Every change with a
// higher seq happened after the list was read, so a client can follow the
// change feed from exactly that point (no gap, no repeats).
func (s *Store) ListAt(ctx context.Context, tenantID int64, prefix string) ([]Entry, int64, error) {
	conn, done, err := s.snapshot(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list %q: %w", prefix, err)
	}
	defer done()

	var seq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM changes`).Scan(&seq); err != nil {
		return nil, 0, fmt.Errorf("list %q: seq: %w", prefix, err)
	}
	rows, err := conn.QueryContext(ctx, `
SELECT key, value, version, updated_at FROM kv
WHERE tenant_id = ? AND substr(key, 1, length(?)) = ?
ORDER BY key`, tenantID, prefix, prefix)
	if err != nil {
		return nil, 0, fmt.Errorf("list %q: %w", prefix, err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ts int64
		if err := rows.Scan(&e.Key, &e.Value, &e.Version, &ts); err != nil {
			return nil, 0, fmt.Errorf("list scan: %w", err)
		}
		e.UpdatedAt = time.Unix(0, ts)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list rows: %w", err)
	}
	return out, seq, nil
}
