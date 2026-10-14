package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func render(v int64) Response {
	return Response{Status: 201, Body: []byte(fmt.Sprintf(`{"version":%d}`, v))}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApplyIdempotentReplayAndConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := tenant(t, s, "a"), tenant(t, s, "b")

	var notified atomic.Int32
	s.AddCommitListener(func(int64) { notified.Add(1) })

	op := Op{Kind: KindPut, Key: "k", Value: []byte(`"v"`)}
	id := &Idem{Key: "idem-1", RequestHash: "hash-1"}

	r1, replayed, err := s.Apply(ctx, a.ID, id, op, render)
	if err != nil || replayed || r1.Status != 201 || string(r1.Body) != `{"version":1}` {
		t.Fatalf("first apply = %+v replayed=%v err=%v", r1, replayed, err)
	}
	r2, replayed, err := s.Apply(ctx, a.ID, id, op, render)
	if err != nil || !replayed || r2.Status != r1.Status || string(r2.Body) != string(r1.Body) {
		t.Fatalf("replay = %+v replayed=%v err=%v", r2, replayed, err)
	}
	if e, _ := s.Get(ctx, a.ID, "k"); e.Version != 1 {
		t.Fatalf("version after replay = %d, want 1", e.Version)
	}
	// Tenant creation is logged too (kind 'tenant'); count key changes only.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM changes WHERE kind = 'key'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("key change rows = %d (%v), want 1", n, err)
	}
	if notified.Load() != 1 {
		t.Fatalf("listeners notified %d times, want 1 (replays must not notify)", notified.Load())
	}

	// Same key, different request hash.
	_, _, err = s.Apply(ctx, a.ID, &Idem{Key: "idem-1", RequestHash: "other"}, op, render)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err = %v", err)
	}

	// Per-tenant scope.
	r3, replayed, err := s.Apply(ctx, b.ID, id, op, render)
	if err != nil || replayed || string(r3.Body) != `{"version":1}` {
		t.Fatalf("tenant b = %+v replayed=%v err=%v", r3, replayed, err)
	}

	// Failed ops record nothing, so the key stays usable.
	del := Op{Kind: KindDelete, Key: "ghost"}
	if _, _, err := s.Apply(ctx, a.ID, &Idem{Key: "idem-2", RequestHash: "h"}, del, render); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete ghost = %v", err)
	}
	if n := countRows(t, s, "idempotency_keys"); n != 2 { // a/idem-1, b/idem-1
		t.Fatalf("idempotency rows = %d, want 2", n)
	}
}

// Regression: a response with no body (e.g. 204 for DELETE) must be storable;
// a nil slice would otherwise be written as NULL into a NOT NULL column.
func TestApplyIdempotentEmptyBodyResponse(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")
	s.Put(ctx, a.ID, "k", []byte("1"))

	id := &Idem{Key: "del", RequestHash: "h"}
	noBody := func(int64) Response { return Response{Status: 204} }
	r, replayed, err := s.Apply(ctx, a.ID, id, Op{Kind: KindDelete, Key: "k"}, noBody)
	if err != nil || replayed || r.Status != 204 {
		t.Fatalf("delete = %+v replayed=%v err=%v", r, replayed, err)
	}
	r, replayed, err = s.Apply(ctx, a.ID, id, Op{Kind: KindDelete, Key: "k"}, noBody)
	if err != nil || !replayed || r.Status != 204 || len(r.Body) != 0 {
		t.Fatalf("replay = %+v replayed=%v err=%v", r, replayed, err)
	}
}

// The idempotency record and the write share one transaction: if either part
// fails, neither is saved. Failures are injected with real SQLite triggers.
func TestApplyIdempotencyIsAtomicWithWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")
	id := &Idem{Key: "idem", RequestHash: "h"}

	// 1. The change-log insert fails: no kv row, no idempotency row.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_change BEFORE INSERT ON changes WHEN NEW.key = 'boom' BEGIN SELECT RAISE(ABORT, 'x'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ctx, a.ID, id, Op{Kind: KindPut, Key: "boom", Value: []byte("1")}, render); err == nil {
		t.Fatal("apply succeeded, want error")
	}
	if _, err := s.Get(ctx, a.ID, "boom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kv row saved despite failure: %v", err)
	}
	if n := countRows(t, s, "idempotency_keys"); n != 0 {
		t.Fatalf("idempotency row saved despite failure: %d", n)
	}
	// With the fault removed the very same key executes normally.
	if _, err := s.db.Exec(`DROP TRIGGER fail_change`); err != nil {
		t.Fatal(err)
	}
	if r, replayed, err := s.Apply(ctx, a.ID, id, Op{Kind: KindPut, Key: "boom", Value: []byte("1")}, render); err != nil || replayed || r.Status != 201 {
		t.Fatalf("retry = %+v replayed=%v err=%v", r, replayed, err)
	}

	// 2. The idempotency insert fails: the kv write and change row roll back.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_idem BEFORE INSERT ON idempotency_keys WHEN NEW.idem_key = 'bad' BEGIN SELECT RAISE(ABORT, 'x'); END`); err != nil {
		t.Fatal(err)
	}
	changesBefore := countRows(t, s, "changes")
	if _, _, err := s.Apply(ctx, a.ID, &Idem{Key: "bad", RequestHash: "h"}, Op{Kind: KindPut, Key: "z", Value: []byte("1")}, render); err == nil {
		t.Fatal("apply succeeded, want error")
	}
	if _, err := s.Get(ctx, a.ID, "z"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kv write survived a failed idempotency insert: %v", err)
	}
	if n := countRows(t, s, "changes"); n != changesBefore {
		t.Fatalf("change row survived a failed idempotency insert: %d -> %d", changesBefore, n)
	}
}
