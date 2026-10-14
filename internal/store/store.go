package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

var (
	ErrNotFound     = errors.New("store: not found")
	ErrTenantExists = errors.New("store: tenant already exists")
	ErrConflict     = errors.New("store: version precondition failed")
)

// Change operations recorded in the change log.
const (
	OpPut    = "put"
	OpDelete = "delete"
)

// Change kinds. Tenant-facing feeds only ever see ChangeKey rows.
const (
	ChangeKey    = "key"    // a write to a tenant's key
	ChangeTenant = "tenant" // creation of a tenant (admin feed only)
)

type Tenant struct {
	ID        int64
	Name      string
	TokenHash string
}

type Entry struct {
	Key       string
	Value     []byte
	Version   int64
	UpdatedAt time.Time
}

// Change is one key change in the append-only change log.
type Change struct {
	Seq       int64
	Key       string
	Op        string
	Version   int64
	Value     []byte // nil for deletes
	ValueSize int64
	TS        time.Time
}

// AdminChange is a change-log row as seen by the admin feed: any tenant, any
// kind. For ChangeTenant rows Key is the tenant name and Value is the JSON
// {"name","token_hash"} (tenant creation, Subject "") or, with Subject
// SubjectQuota, the JSON {"max_keys","max_bytes","version"}.
type AdminChange struct {
	Change
	TenantID int64
	Kind     string
	Subject  string // "" or SubjectQuota (ChangeTenant rows only)
}

type Store struct {
	db  *sql.DB // read pool
	wdb *sql.DB // single-connection write pool

	mu        sync.RWMutex
	listeners []func(tenantID int64)
}

type openConfig struct{ foreignKeys bool }

// Option customizes Open.
type Option func(*openConfig)

// WithoutForeignKeys turns foreign-key enforcement off. Replicas use it: they
// replay the primary's log in order and must tolerate a legacy primary whose
// tenant records come after its first key changes (see migration 4).
func WithoutForeignKeys() Option { return func(c *openConfig) { c.foreignKeys = false } }

// Open opens (creating if needed) the SQLite file at path in WAL mode with a
// busy timeout, and applies pending migrations.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	cfg := openConfig{foreignKeys: true}
	for _, o := range opts {
		o(&cfg)
	}
	fk := "0"
	if cfg.foreignKeys {
		fk = "1"
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys("+fk+")")
	q.Add("_txlock", "immediate") // take the write lock at BEGIN so busy_timeout applies
	dsn := "file:" + path + "?" + q.Encode()
	// All writes go through one connection so they queue in Go; SQLite's busy
	// handler backs off with sleeps and is very slow under many contending
	// writers. Reads use a separate pool and run concurrently under WAL.
	wdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	wdb.SetMaxOpenConns(1)
	if err := wdb.PingContext(ctx); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	if err := migrate(ctx, wdb); err != nil {
		wdb.Close()
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		wdb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db, wdb: wdb}, nil
}

func (s *Store) Close() error {
	return errors.Join(s.db.Close(), s.wdb.Close())
}

// AddCommitListener registers fn to be called (synchronously, after commit)
// with the tenant ID whenever a write to that tenant's data commits.
func (s *Store) AddCommitListener(fn func(tenantID int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

func (s *Store) notify(tenantID int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, fn := range s.listeners {
		fn(tenantID)
	}
}

// recordChange appends one row to the change log inside tx. kind is ChangeKey
// or ChangeTenant; ts is Unix nanoseconds; subject is "" (stored as NULL) or
// SubjectQuota.
func recordChange(ctx context.Context, tx *sql.Tx, tenantID int64, kind, key, op string, version int64, value []byte, ts int64, subject string) error {
	var v any // NULL for deletes
	size := int64(0)
	if op == OpPut {
		v = string(value)
		size = int64(len(value))
	}
	var subj any // NULL unless set
	if subject != "" {
		subj = subject
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO changes(tenant_id, kind, key, op, version, value, value_size, ts, subject) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, kind, key, op, version, v, size, ts, subj)
	if err != nil {
		return fmt.Errorf("record change %q: %w", key, err)
	}
	return nil
}

// tenantRecord is the JSON stored as the value of a tenant change row.
type tenantRecord struct {
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"`
}

// CreateTenant adds a tenant and, in the same transaction, logs its creation
// (a ChangeTenant change row) so replicas can learn about it.
func (s *Store) CreateTenant(ctx context.Context, name, tokenHash string) (Tenant, error) {
	var t Tenant
	err := s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		res, err := tx.ExecContext(ctx, `INSERT INTO tenants(name, token_hash) VALUES (?, ?)`, name, tokenHash)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return 0, false, ErrTenantExists
			}
			return 0, false, fmt.Errorf("create tenant: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("create tenant id: %w", err)
		}
		rec, err := json.Marshal(tenantRecord{Name: name, TokenHash: tokenHash})
		if err != nil {
			return 0, false, fmt.Errorf("encode tenant record: %w", err)
		}
		if err := recordChange(ctx, tx, id, ChangeTenant, name, OpPut, 1, rec, time.Now().UnixNano(), ""); err != nil {
			return 0, false, err
		}
		t = Tenant{ID: id, Name: name, TokenHash: tokenHash}
		return id, true, nil
	})
	if err != nil {
		return Tenant{}, err
	}
	return t, nil
}

func (s *Store) TenantByTokenHash(ctx context.Context, tokenHash string) (Tenant, error) {
	var t Tenant
	err := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash FROM tenants WHERE token_hash = ?`, tokenHash).
		Scan(&t.ID, &t.Name, &t.TokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant by token hash: %w", err)
	}
	return t, nil
}

func (s *Store) Get(ctx context.Context, tenantID int64, key string) (Entry, error) {
	var e Entry
	var ts int64
	err := s.db.QueryRowContext(ctx,
		`SELECT key, value, version, updated_at FROM kv WHERE tenant_id = ? AND key = ?`, tenantID, key).
		Scan(&e.Key, &e.Value, &e.Version, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("get %q: %w", key, err)
	}
	e.UpdatedAt = time.Unix(0, ts)
	return e, nil
}

// List returns the tenant's entries whose key starts with prefix, ordered by key.
func (s *Store) List(ctx context.Context, tenantID int64, prefix string) ([]Entry, error) {
	entries, _, err := s.ListAt(ctx, tenantID, prefix)
	return entries, err
}

// Changes returns the tenant's key changes with seq > since in ascending
// order, at most limit of them. It never returns another tenant's changes, nor
// tenant records (kind 'tenant'), which belong to the admin feed only.
func (s *Store) Changes(ctx context.Context, tenantID, since int64, limit int) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, key, op, version, value, value_size, ts FROM changes
WHERE tenant_id = ? AND kind = 'key' AND seq > ?
ORDER BY seq LIMIT ?`, tenantID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("changes since %d: %w", since, err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var val sql.NullString
		var ts int64
		if err := rows.Scan(&c.Seq, &c.Key, &c.Op, &c.Version, &val, &c.ValueSize, &ts); err != nil {
			return nil, fmt.Errorf("changes scan: %w", err)
		}
		if val.Valid {
			c.Value = []byte(val.String)
		}
		c.TS = time.Unix(0, ts).UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("changes rows: %w", err)
	}
	return out, nil
}

// AdminChanges returns changes of every tenant and kind with seq > since in
// ascending order, at most limit of them. It is the replication feed.
func (s *Store) AdminChanges(ctx context.Context, since int64, limit int) ([]AdminChange, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, tenant_id, kind, key, op, version, value, value_size, ts, COALESCE(subject, '') FROM changes
WHERE seq > ?
ORDER BY seq LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("admin changes since %d: %w", since, err)
	}
	defer rows.Close()
	var out []AdminChange
	for rows.Next() {
		var c AdminChange
		var val sql.NullString
		var ts int64
		if err := rows.Scan(&c.Seq, &c.TenantID, &c.Kind, &c.Key, &c.Op, &c.Version, &val, &c.ValueSize, &ts, &c.Subject); err != nil {
			return nil, fmt.Errorf("admin changes scan: %w", err)
		}
		if val.Valid {
			c.Value = []byte(val.String)
		}
		c.TS = time.Unix(0, ts).UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin changes rows: %w", err)
	}
	return out, nil
}

// LatestSeq returns the highest seq in this database's change log (0 if empty).
func (s *Store) LatestSeq(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM changes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("latest seq: %w", err)
	}
	return n, nil
}
