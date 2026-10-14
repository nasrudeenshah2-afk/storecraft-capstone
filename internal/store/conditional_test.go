package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestPutIfVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	if _, err := s.PutIfVersion(ctx, a.ID, "k", []byte("x"), 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing key = %v, want ErrConflict", err)
	}
	s.Put(ctx, a.ID, "k", []byte("v1"))
	v, err := s.PutIfVersion(ctx, a.ID, "k", []byte("v2"), 1)
	if err != nil || v != 2 {
		t.Fatalf("CAS = %d, %v; want 2", v, err)
	}
	if _, err := s.PutIfVersion(ctx, a.ID, "k", []byte("v3"), 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS = %v, want ErrConflict", err)
	}
	if e, _ := s.Get(ctx, a.ID, "k"); string(e.Value) != "v2" || e.Version != 2 {
		t.Fatalf("after failed CAS = %+v", e)
	}
}

func TestPutIfAbsent(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	if v, err := s.PutIfAbsent(ctx, a.ID, "k", []byte("1")); err != nil || v != 1 {
		t.Fatalf("create = %d, %v", v, err)
	}
	if _, err := s.PutIfAbsent(ctx, a.ID, "k", []byte("2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create = %v, want ErrConflict", err)
	}
	if e, _ := s.Get(ctx, a.ID, "k"); string(e.Value) != "1" {
		t.Fatalf("value overwritten: %q", e.Value)
	}
}

// Exactly one concurrent caller may win each conditional write.
func TestConditionalWritesRace(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	race := func(name string, fn func() error) {
		const n = 50
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := fn()
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case !errors.Is(err, ErrConflict):
					t.Errorf("%s: unexpected error %v", name, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%s: %d winners, want 1", name, wins)
		}
	}
	race("create", func() error { _, err := s.PutIfAbsent(ctx, a.ID, "new", []byte("x")); return err })
	race("cas", func() error { _, err := s.PutIfVersion(ctx, a.ID, "new", []byte("y"), 1); return err })
}
