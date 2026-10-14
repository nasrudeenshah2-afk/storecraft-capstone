package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	version int
	sql     string
}

// migrations is append-only: never edit or reorder an applied entry.
var migrations = []migration{
	{1, `
CREATE TABLE tenants (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE
);
CREATE TABLE kv (
	tenant_id  INTEGER NOT NULL REFERENCES tenants(id),
	key        TEXT NOT NULL,
	value      BLOB NOT NULL,
	version    INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, key)
);`},
	{2, `
CREATE TABLE changes (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id  INTEGER NOT NULL REFERENCES tenants(id),
	key        TEXT NOT NULL,
	op         TEXT NOT NULL CHECK (op IN ('put', 'delete')),
	version    INTEGER NOT NULL,
	value      TEXT,
	value_size INTEGER NOT NULL,
	ts         INTEGER NOT NULL,
	CHECK ((op = 'put') = (value IS NOT NULL))
);
CREATE INDEX idx_changes_tenant_seq ON changes(tenant_id, seq);`},
	{3, `
CREATE TABLE idempotency_keys (
	tenant_id     INTEGER NOT NULL REFERENCES tenants(id),
	idem_key      TEXT NOT NULL,
	request_hash  TEXT NOT NULL,
	status_code   INTEGER NOT NULL,
	response_body BLOB NOT NULL,
	created_at    INTEGER NOT NULL,
	UNIQUE (tenant_id, idem_key)
);`},
	// 4: tenant creation becomes a logged change (kind 'tenant') so a replica
	// can learn tenants from the log. Additive: one new column (existing rows
	// default to 'key') plus a backfill row per pre-existing tenant. On a
	// database that already had data, those rows land at the end of the log.
	{4, `
ALTER TABLE changes ADD COLUMN kind TEXT NOT NULL DEFAULT 'key' CHECK (kind IN ('key', 'tenant'));
INSERT INTO changes(tenant_id, kind, key, op, version, value, value_size, ts)
SELECT id, 'tenant', name, 'put', 1,
	json_object('name', name, 'token_hash', token_hash),
	length(CAST(json_object('name', name, 'token_hash', token_hash) AS BLOB)),
	CAST(strftime('%s', 'now') AS INTEGER) * 1000000000
FROM tenants ORDER BY id;`},
	// 5: replica bookkeeping. A row exists only on replica databases;
	// applied_seq is the PRIMARY's seq of the last change applied here (never
	// this database's own change seq).
	{5, `
CREATE TABLE replica_state (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	applied_seq INTEGER NOT NULL
);`},
	// 6 (STORE-210): quotas, usage, flags. Additive only - nothing is dropped,
	// renamed or tightened, so the v1.0.0 binary still runs on this schema.
	//   - tenant_quotas: one row per EDITED quota; no row = the default quota
	//     at version 0. NULL limit = unlimited.
	//   - tenant_usage: kept equal to COUNT(*) / SUM(value bytes) of kv by the
	//     triggers below, inside the same transaction as every insert, update
	//     or delete of kv - whoever writes (this build, v1.0.0, a replica).
	//     The triggers avoid INSERT OR IGNORE: inside a trigger, the conflict
	//     policy of the outer statement (e.g. an UPSERT) would override it.
	//   - flags: runtime feature flags; no row = off at version 0.
	//   - backfill_progress: tenants done by an unfinished backfill-usage run.
	//   - changes.subject: what a 'tenant' change row is about (NULL =
	//     tenant creation, 'quota' = a quota change). Older code ignores it.
	{6, `
CREATE TABLE tenant_quotas (
	tenant_id  INTEGER PRIMARY KEY REFERENCES tenants(id),
	max_keys   INTEGER CHECK (max_keys IS NULL OR max_keys >= 0),
	max_bytes  INTEGER CHECK (max_bytes IS NULL OR max_bytes >= 0),
	version    INTEGER NOT NULL CHECK (version >= 1),
	updated_at INTEGER NOT NULL
);
CREATE TABLE tenant_usage (
	tenant_id  INTEGER PRIMARY KEY,
	key_count  INTEGER NOT NULL,
	byte_count INTEGER NOT NULL
);
CREATE TABLE flags (
	name       TEXT PRIMARY KEY,
	enabled    INTEGER NOT NULL CHECK (enabled IN (0, 1)),
	version    INTEGER NOT NULL CHECK (version >= 1),
	updated_at INTEGER NOT NULL
);
CREATE TABLE backfill_progress (
	tenant_id    INTEGER PRIMARY KEY,
	completed_at INTEGER NOT NULL
);
ALTER TABLE changes ADD COLUMN subject TEXT;
INSERT INTO tenant_usage(tenant_id, key_count, byte_count)
SELECT tenant_id, COUNT(*), SUM(length(CAST(value AS BLOB))) FROM kv GROUP BY tenant_id;
CREATE TRIGGER kv_usage_insert AFTER INSERT ON kv BEGIN
	INSERT INTO tenant_usage(tenant_id, key_count, byte_count)
	SELECT NEW.tenant_id, 0, 0 WHERE NOT EXISTS (SELECT 1 FROM tenant_usage WHERE tenant_id = NEW.tenant_id);
	UPDATE tenant_usage SET key_count = key_count + 1, byte_count = byte_count + length(CAST(NEW.value AS BLOB))
	WHERE tenant_id = NEW.tenant_id;
END;
CREATE TRIGGER kv_usage_update AFTER UPDATE OF tenant_id, value ON kv BEGIN
	UPDATE tenant_usage SET key_count = key_count - 1, byte_count = byte_count - length(CAST(OLD.value AS BLOB))
	WHERE tenant_id = OLD.tenant_id;
	INSERT INTO tenant_usage(tenant_id, key_count, byte_count)
	SELECT NEW.tenant_id, 0, 0 WHERE NOT EXISTS (SELECT 1 FROM tenant_usage WHERE tenant_id = NEW.tenant_id);
	UPDATE tenant_usage SET key_count = key_count + 1, byte_count = byte_count + length(CAST(NEW.value AS BLOB))
	WHERE tenant_id = NEW.tenant_id;
END;
CREATE TRIGGER kv_usage_delete AFTER DELETE ON kv BEGIN
	UPDATE tenant_usage SET key_count = key_count - 1, byte_count = byte_count - length(CAST(OLD.value AS BLOB))
	WHERE tenant_id = OLD.tenant_id;
END;`},
}

func migrate(ctx context.Context, db *sql.DB) error {
	return migrateTo(ctx, db, len(migrations))
}

// migrateTo applies pending migrations up to and including version max.
func migrateTo(ctx context.Context, db *sql.DB, max int) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range migrations {
		if m.version > max {
			break
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if n > 0 {
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %d: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, m.version, time.Now().Unix()); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	return nil
}
