package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrIdempotencyConflict means an idempotency key was reused for a different
// request (different method, path or body).
var ErrIdempotencyConflict = errors.New("store: idempotency key reused with a different request")

type OpKind int

const (
	KindPut          OpKind = iota // upsert
	KindPutIfVersion               // compare-and-swap on Expected
	KindPutIfAbsent                // create only
	KindDelete
)

// Op is one write to a tenant's key.
type Op struct {
	Kind     OpKind
	Key      string
	Value    []byte
	Expected int64 // KindPutIfVersion only
}

// Idem identifies a request for idempotent replay. RequestHash must cover
// everything that makes two requests "the same" (method, path, body).
type Idem struct {
	Key         string
	RequestHash string
}

// Response is a stored HTTP response (status and body).
type Response struct {
	Status int
	Body   []byte
}

// writeTx runs fn in one transaction. If fn returns notify=true and the
// commit succeeds, listeners are told that data of tenantID changed (fn
// returns the id because it may only be known inside the transaction). If fn
// or the commit fails nothing is saved.
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) (tenantID int64, notify bool, err error)) error {
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	tenantID, notify, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if notify {
		s.notify(tenantID)
	}
	return nil
}

// Apply performs op, writing its change-log row in the same transaction.
//
// With idem == nil it is a plain write. With idem set, the idempotency record
// is checked and written in the same transaction as the write: a stored record
// with the same request hash returns the stored response (replayed == true)
// without writing or notifying; a different hash returns
// ErrIdempotencyConflict; otherwise the op runs, render(version) produces the
// response, and it is stored together with the write. Failed ops store
// nothing, so a retry re-evaluates.
func (s *Store) Apply(ctx context.Context, tenantID int64, idem *Idem, op Op, render func(version int64) Response) (resp Response, replayed bool, err error) {
	err = s.writeTx(ctx, func(tx *sql.Tx) (int64, bool, error) {
		if idem != nil {
			var hash string
			var stored Response
			err := tx.QueryRowContext(ctx,
				`SELECT request_hash, status_code, response_body FROM idempotency_keys WHERE tenant_id = ? AND idem_key = ?`,
				tenantID, idem.Key).Scan(&hash, &stored.Status, &stored.Body)
			switch {
			case err == nil:
				if hash != idem.RequestHash {
					return 0, false, ErrIdempotencyConflict
				}
				resp, replayed = stored, true
				return 0, false, nil
			case !errors.Is(err, sql.ErrNoRows):
				return 0, false, fmt.Errorf("idempotency lookup: %w", err)
			}
		}

		version, err := execOp(ctx, tx, tenantID, op)
		if err != nil {
			return 0, false, err
		}
		if render != nil {
			resp = render(version)
		}
		if idem != nil {
			body := resp.Body
			if body == nil {
				body = []byte{} // a nil slice would be stored as NULL
			}
			_, err := tx.ExecContext(ctx, `
INSERT INTO idempotency_keys(tenant_id, idem_key, request_hash, status_code, response_body, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, tenantID, idem.Key, idem.RequestHash, resp.Status, body, time.Now().UnixNano())
			if err != nil {
				return 0, false, fmt.Errorf("store idempotency record: %w", err)
			}
		}
		return tenantID, true, nil
	})
	if err != nil {
		return Response{}, false, err
	}
	return resp, replayed, nil
}

// execOp runs the kv statement for op plus its change row, returning the
// resulting version (the deleted version for deletes).
//
// Puts store the value in canonical form (CanonicalJSON). Before writing, a
// put checks, in this transaction and in this order: the version
// precondition (ErrConflict, the 412), then the quota (a *QuotaError, the
// 403) for what the write would add - one key if the key is new, and the new
// minus the old value bytes. The usage counters themselves are updated by the
// kv triggers (migration 6) in the same transaction. Deletes are never
// checked: they only free space.
func execOp(ctx context.Context, tx *sql.Tx, tenantID int64, op Op) (int64, error) {
	value := CanonicalJSON(op.Value)
	if value == nil {
		value = []byte{}
	}
	if op.Kind != KindDelete {
		var oldVersion, oldBytes int64
		err := tx.QueryRowContext(ctx, `SELECT version, length(CAST(value AS BLOB)) FROM kv WHERE tenant_id = ? AND key = ?`,
			tenantID, op.Key).Scan(&oldVersion, &oldBytes)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read %q: %w", op.Key, err)
		}
		switch {
		case op.Kind == KindPutIfVersion && (!exists || oldVersion != op.Expected):
			return 0, ErrConflict
		case op.Kind == KindPutIfAbsent && exists:
			return 0, ErrConflict
		}
		addKeys, addBytes := int64(1), int64(len(value))
		if exists {
			addKeys, addBytes = 0, int64(len(value))-oldBytes
		}
		if err := checkQuota(ctx, tx, tenantID, addKeys, addBytes); err != nil {
			return 0, err
		}
	}
	var version int64
	switch op.Kind {
	case KindPut:
		err := tx.QueryRowContext(ctx, `
INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, ?, ?, 1, ?)
ON CONFLICT(tenant_id, key) DO UPDATE SET
	value = excluded.value,
	version = kv.version + 1,
	updated_at = excluded.updated_at
RETURNING version`, tenantID, op.Key, value, time.Now().UnixNano()).Scan(&version)
		if err != nil {
			return 0, fmt.Errorf("put %q: %w", op.Key, err)
		}
	case KindPutIfVersion:
		err := tx.QueryRowContext(ctx, `
UPDATE kv SET value = ?, version = version + 1, updated_at = ?
WHERE tenant_id = ? AND key = ? AND version = ?
RETURNING version`, value, time.Now().UnixNano(), tenantID, op.Key, op.Expected).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrConflict
		}
		if err != nil {
			return 0, fmt.Errorf("put-if-version %q: %w", op.Key, err)
		}
	case KindPutIfAbsent:
		err := tx.QueryRowContext(ctx, `
INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, ?, ?, 1, ?)
ON CONFLICT(tenant_id, key) DO NOTHING
RETURNING version`, tenantID, op.Key, value, time.Now().UnixNano()).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrConflict
		}
		if err != nil {
			return 0, fmt.Errorf("put-if-absent %q: %w", op.Key, err)
		}
	case KindDelete:
		err := tx.QueryRowContext(ctx,
			`DELETE FROM kv WHERE tenant_id = ? AND key = ? RETURNING version`, tenantID, op.Key).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("delete %q: %w", op.Key, err)
		}
		return version, recordChange(ctx, tx, tenantID, ChangeKey, op.Key, OpDelete, version, nil, time.Now().UnixNano(), "")
	default:
		return 0, fmt.Errorf("unknown op kind %d", op.Kind)
	}
	return version, recordChange(ctx, tx, tenantID, ChangeKey, op.Key, OpPut, version, value, time.Now().UnixNano(), "")
}

// Put upserts key and returns its new version (1 for a new key).
func (s *Store) Put(ctx context.Context, tenantID int64, key string, value []byte) (int64, error) {
	return s.applyVersion(ctx, tenantID, Op{Kind: KindPut, Key: key, Value: value})
}

// PutIfVersion overwrites key only if its current version equals expected
// (compare-and-swap). It returns ErrConflict if the key is missing or at a
// different version.
func (s *Store) PutIfVersion(ctx context.Context, tenantID int64, key string, value []byte, expected int64) (int64, error) {
	return s.applyVersion(ctx, tenantID, Op{Kind: KindPutIfVersion, Key: key, Value: value, Expected: expected})
}

// PutIfAbsent creates key at version 1 only if it does not exist. It returns
// ErrConflict if the key already exists.
func (s *Store) PutIfAbsent(ctx context.Context, tenantID int64, key string, value []byte) (int64, error) {
	return s.applyVersion(ctx, tenantID, Op{Kind: KindPutIfAbsent, Key: key, Value: value})
}

// Delete removes key; it returns ErrNotFound if the key does not exist. The
// change row (recording the deleted version) is written in the same transaction.
func (s *Store) Delete(ctx context.Context, tenantID int64, key string) error {
	_, err := s.applyVersion(ctx, tenantID, Op{Kind: KindDelete, Key: key})
	return err
}

func (s *Store) applyVersion(ctx context.Context, tenantID int64, op Op) (int64, error) {
	var version int64
	_, _, err := s.Apply(ctx, tenantID, nil, op, func(v int64) Response {
		version = v
		return Response{}
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}
