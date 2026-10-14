package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
)

func countChanges(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChangeRowPerOperation(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	s.Put(ctx, a.ID, "k", []byte(`"one"`))               // v1
	s.Put(ctx, a.ID, "k", []byte(`"two"`))               // v2
	s.PutIfVersion(ctx, a.ID, "k", []byte(`"three"`), 2) // v3
	s.PutIfAbsent(ctx, a.ID, "n", []byte(`{"x":1}`))     // v1 of n
	if err := s.Delete(ctx, a.ID, "k"); err != nil {     // deletes v3
		t.Fatal(err)
	}
	// Operations that fail their precondition must not log anything.
	if _, err := s.PutIfVersion(ctx, a.ID, "n", []byte("z"), 9); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, a.ID, "n", []byte("z")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, a.ID, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	got, err := s.Changes(ctx, a.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key, op, value string
		version, size  int64
	}{
		{"k", OpPut, `"one"`, 1, 5},
		{"k", OpPut, `"two"`, 2, 5},
		{"k", OpPut, `"three"`, 3, 7},
		{"n", OpPut, `{"x":1}`, 1, 7},
		{"k", OpDelete, "", 3, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		c := got[i]
		if c.Key != w.key || c.Op != w.op || c.Version != w.version || c.ValueSize != w.size || c.TS.IsZero() {
			t.Fatalf("change %d = %+v, want %+v", i, c, w)
		}
		if w.op == OpDelete {
			if c.Value != nil {
				t.Fatalf("delete change has value %q", c.Value)
			}
		} else if string(c.Value) != w.value {
			t.Fatalf("change %d value = %q, want %q", i, c.Value, w.value)
		}
		if i > 0 && c.Seq <= got[i-1].Seq {
			t.Fatalf("seq not ascending: %+v", got)
		}
	}
	// since/limit.
	page, _ := s.Changes(ctx, a.ID, got[1].Seq, 2)
	if len(page) != 2 || page[0].Seq != got[2].Seq {
		t.Fatalf("since/limit page = %+v", page)
	}
}

func TestChangesAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := tenant(t, s, "a"), tenant(t, s, "b")
	s.Put(ctx, a.ID, "secret", []byte(`"a"`))
	s.Put(ctx, b.ID, "mine", []byte(`"b"`))
	s.Delete(ctx, a.ID, "secret")

	got, _ := s.Changes(ctx, b.ID, 0, 100)
	if len(got) != 1 || got[0].Key != "mine" {
		t.Fatalf("b sees %+v", got)
	}
	got, _ = s.Changes(ctx, a.ID, 0, 100)
	if len(got) != 2 {
		t.Fatalf("a sees %+v", got)
	}
}

// A write whose change-log insert fails must leave neither a kv change nor a
// change row. The failure is injected with a real SQLite trigger.
func TestFailedWriteLeavesNoChangeRow(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	// Existing data to protect.
	s.Put(ctx, a.ID, "upd", []byte(`"v1"`))
	s.Put(ctx, a.ID, "keepme", []byte(`"k"`))
	before := countChanges(t, s)

	for _, trig := range []string{
		`CREATE TRIGGER fail_create BEFORE INSERT ON changes WHEN NEW.key = 'boom' BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`CREATE TRIGGER fail_update BEFORE INSERT ON changes WHEN NEW.key = 'upd' AND NEW.version = 2 BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`CREATE TRIGGER fail_delete BEFORE INSERT ON changes WHEN NEW.key = 'keepme' AND NEW.op = 'delete' BEGIN SELECT RAISE(ABORT, 'injected'); END`,
	} {
		if _, err := s.db.Exec(trig); err != nil {
			t.Fatal(err)
		}
	}

	// Create fails: no kv row.
	if _, err := s.Put(ctx, a.ID, "boom", []byte(`"x"`)); err == nil {
		t.Fatal("Put boom succeeded, want error")
	}
	if _, err := s.Get(ctx, a.ID, "boom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kv row for failed create exists: %v", err)
	}
	if _, err := s.PutIfAbsent(ctx, a.ID, "boom", []byte(`"x"`)); err == nil {
		t.Fatal("PutIfAbsent boom succeeded, want error")
	}
	if _, err := s.Get(ctx, a.ID, "boom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kv row for failed put-if-absent exists: %v", err)
	}

	// Update fails: value and version unchanged.
	if _, err := s.Put(ctx, a.ID, "upd", []byte(`"v2"`)); err == nil {
		t.Fatal("update succeeded, want error")
	}
	if _, err := s.PutIfVersion(ctx, a.ID, "upd", []byte(`"v2"`), 1); err == nil {
		t.Fatal("CAS update succeeded, want error")
	}
	if e, _ := s.Get(ctx, a.ID, "upd"); string(e.Value) != `"v1"` || e.Version != 1 {
		t.Fatalf("kv changed by failed update: %+v", e)
	}

	// Delete fails: row still there.
	if err := s.Delete(ctx, a.ID, "keepme"); err == nil {
		t.Fatal("delete succeeded, want error")
	}
	if e, err := s.Get(ctx, a.ID, "keepme"); err != nil || string(e.Value) != `"k"` {
		t.Fatalf("kv row lost by failed delete: %+v, %v", e, err)
	}

	if after := countChanges(t, s); after != before {
		t.Fatalf("change rows %d -> %d, want unchanged", before, after)
	}

	// Rolled-back writes must not burn sequence numbers.
	last, _ := s.Changes(ctx, a.ID, 0, 100)
	prev := last[len(last)-1].Seq
	if _, err := s.Put(ctx, a.ID, "fine", []byte(`"ok"`)); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Changes(ctx, a.ID, prev, 10)
	if len(next) != 1 || next[0].Seq != prev+1 {
		t.Fatalf("seq after failures = %+v, want %d", next, prev+1)
	}
}

func TestConcurrentWritesGiveUniqueConsecutiveSeq(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := string(rune('a' + i%5))
			if _, err := s.Put(ctx, a.ID, key, []byte(`1`)); err != nil {
				t.Errorf("put: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	got, err := s.Changes(ctx, a.ID, 0, 500)
	if err != nil || len(got) != n {
		t.Fatalf("changes = %d, %v; want %d", len(got), err, n)
	}
	seqs := make([]int64, n)
	for i, c := range got {
		seqs[i] = c.Seq
	}
	if !sort.SliceIsSorted(seqs, func(i, j int) bool { return seqs[i] < seqs[j] }) {
		t.Fatalf("seqs not ascending: %v", seqs)
	}
	for i := 1; i < n; i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("gap or duplicate in seqs: %v", seqs)
		}
	}
}
