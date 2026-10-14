package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// busyRetryFor bounds how long one backfill transaction keeps retrying while
// another process (the server) holds the write lock.
const busyRetryFor = 2 * time.Minute

// isBusy reports whether err is SQLite's "database is locked" (SQLITE_BUSY).
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// writeTxRetryBusy is writeTx for a separate process such as backfill-usage.
// SQLite's busy handler is not fair: under a steady stream of writes from the
// server, a second process can wait longer than busy_timeout and get
// SQLITE_BUSY. The transaction is then retried with jittered backoff (it has
// not changed anything) until busyRetryFor has passed.
func (s *Store) writeTxRetryBusy(ctx context.Context, fn func(tx *sql.Tx) (int64, bool, error)) error {
	deadline := time.Now().Add(busyRetryFor)
	wait := 10 * time.Millisecond
	for {
		err := s.writeTx(ctx, fn)
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		t := time.NewTimer(wait/2 + rand.N(wait/2+1))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, time.Second)
	}
}

// BackfillOptions configures BackfillUsage.
type BackfillOptions struct {
	// Restart forgets the progress of an earlier, unfinished run.
	Restart bool
	// Limit stops after this many tenants (0 = no limit). The run can be
	// continued later; used to stop a large backfill in slices.
	Limit int
}

// BackfillResult reports what one BackfillUsage call did.
type BackfillResult struct {
	Processed   int   // tenants processed by this call
	AlreadyDone int   // tenants skipped because an earlier unfinished run did them
	Remaining   int   // tenants still to do (0 = the backfill is complete)
	Rewritten   int64 // values rewritten in canonical form by this call
}

// BackfillUsage repairs every tenant's usage counters and stores old values in
// canonical form (STORE-210, AC-API-13). It is idempotent and resumable:
//
//   - Each tenant's values are canonicalised in short transactions of
//     canonicaliseBatch values; then ONE write transaction (BEGIN IMMEDIATE)
//     SETS tenant_usage to COUNT(*) and SUM(value bytes) of its kv rows and
//     records the tenant as done - it never adds to the old numbers, so
//     running it again cannot double count.
//   - A live write to the same database (another process included) waits for
//     that transaction or runs before it; the kv triggers keep the counters
//     correct for every write before and after, so there is no race window.
//     When the server keeps the write lock busy, a transaction that got
//     SQLITE_BUSY is retried (see writeTxRetryBusy).
//   - backfill_progress records finished tenants in the same transaction. A
//     stopped run (Limit, a crash, Ctrl+C) continues where it stopped; when a
//     run completes the progress is cleared, so the next run starts afresh.
//
// Rewriting a value in canonical form keeps its version and logs no change:
// the JSON value is the same, only its spelling changes.
func (s *Store) BackfillUsage(ctx context.Context, opt BackfillOptions) (BackfillResult, error) {
	var res BackfillResult
	if opt.Restart {
		if err := s.writeTxRetryBusy(ctx, func(tx *sql.Tx) (int64, bool, error) {
			_, err := tx.ExecContext(ctx, `DELETE FROM backfill_progress`)
			return 0, false, err
		}); err != nil {
			return res, fmt.Errorf("backfill: restart: %w", err)
		}
	}
	ids, err := s.backfillTenants(ctx)
	if err != nil {
		return res, err
	}
	for i, id := range ids {
		if opt.Limit > 0 && res.Processed >= opt.Limit {
			res.Remaining = len(ids) - i
			return res, nil
		}
		var done bool
		var rewritten int64
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backfill_progress WHERE tenant_id = ?`, id).Scan(&n); err != nil {
			return res, fmt.Errorf("backfill: tenant %d: %w", id, err)
		}
		if n == 0 {
			var err error
			if rewritten, err = s.canonicaliseTenant(ctx, id); err != nil {
				return res, fmt.Errorf("backfill: tenant %d: %w", id, err)
			}
		}
		err := s.writeTxRetryBusy(ctx, func(tx *sql.Tx) (int64, bool, error) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backfill_progress WHERE tenant_id = ?`, id).Scan(&n); err != nil {
				return 0, false, err
			}
			if n > 0 {
				done = true
				return 0, false, nil
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_usage WHERE tenant_id = ?`, id); err != nil {
				return 0, false, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_usage(tenant_id, key_count, byte_count)
SELECT ?, COUNT(*), COALESCE(SUM(length(CAST(value AS BLOB))), 0) FROM kv WHERE tenant_id = ?`, id, id); err != nil {
				return 0, false, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO backfill_progress(tenant_id, completed_at) VALUES (?, ?)`, id, time.Now().UnixNano())
			return 0, false, err
		})
		if err != nil {
			return res, fmt.Errorf("backfill: tenant %d: %w", id, err)
		}
		if done {
			res.AlreadyDone++
		} else {
			res.Processed++
			res.Rewritten += rewritten
		}
	}
	// Complete: forget the progress so the next run checks everything again.
	if err := s.writeTxRetryBusy(ctx, func(tx *sql.Tx) (int64, bool, error) {
		_, err := tx.ExecContext(ctx, `DELETE FROM backfill_progress`)
		return 0, false, err
	}); err != nil {
		return res, fmt.Errorf("backfill: finish: %w", err)
	}
	return res, nil
}

// backfillTenants lists every tenant id that has a tenant row, keys, or a
// usage row (a stale counter for a tenant without keys must be reset too).
func (s *Store) backfillTenants(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM tenants
UNION SELECT tenant_id FROM kv
UNION SELECT tenant_id FROM tenant_usage
ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("backfill: list tenants: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("backfill: list tenants: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// canonicaliseBatch is how many values one canonicalisation transaction
// reads and rewrites. Small batches keep the write lock short, so the
// server's writes never wait long for a backfill (review finding R2: one
// transaction per tenant held the lock for 36 s for 200k values).
const canonicaliseBatch = 100

// canonicaliseTenant rewrites the tenant's non-canonical values in place, in
// key order, canonicaliseBatch values per transaction. After each batch it
// pauses at least as long as the batch held the lock: SQLite's busy handler
// is not fair, and a writer in another process (the server) sleeping in it
// would otherwise keep missing the short gaps between batches. Writes between batches
// are fine: the triggers keep the counters right, new values are already
// canonical, and the final step of BackfillUsage recomputes the counters.
func (s *Store) canonicaliseTenant(ctx context.Context, tenantID int64) (int64, error) {
	var total int64
	after := ""
	for {
		var fixed, seen int
		start := time.Now()
		err := s.writeTxRetryBusy(ctx, func(tx *sql.Tx) (int64, bool, error) {
			fixed, seen = 0, 0
			rows, err := tx.QueryContext(ctx, `SELECT key, value FROM kv WHERE tenant_id = ? AND key > ? ORDER BY key LIMIT ?`,
				tenantID, after, canonicaliseBatch)
			if err != nil {
				return 0, false, err
			}
			type fix struct {
				key   string
				value []byte
			}
			var fixes []fix
			last := after
			for rows.Next() {
				var key string
				var value []byte
				if err := rows.Scan(&key, &value); err != nil {
					rows.Close()
					return 0, false, err
				}
				seen++
				last = key
				if c := CanonicalJSON(value); !bytes.Equal(c, value) {
					fixes = append(fixes, fix{key, c})
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, false, err
			}
			for _, f := range fixes {
				if _, err := tx.ExecContext(ctx, `UPDATE kv SET value = ? WHERE tenant_id = ? AND key = ?`, f.value, tenantID, f.key); err != nil {
					return 0, false, err
				}
			}
			fixed = len(fixes)
			after = last
			return 0, false, nil
		})
		if err != nil {
			return total, err
		}
		total += int64(fixed)
		if seen < canonicaliseBatch {
			return total, nil
		}
		pause := time.NewTimer(time.Since(start) + 10*time.Millisecond)
		select {
		case <-ctx.Done():
			pause.Stop()
			return total, ctx.Err()
		case <-pause.C:
		}
	}
}
