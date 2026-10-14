package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestListAtReturnsTheSeqOfTheSameSnapshot(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a") // seq 1
	s.Put(ctx, a.ID, "k1", []byte(`1`))
	s.Put(ctx, a.ID, "k2", []byte(`2`))

	entries, seq, err := s.ListAt(ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	latest, _ := s.LatestSeq(ctx)
	if len(entries) != 2 || seq != latest || seq != 3 {
		t.Fatalf("ListAt = %d entries at seq %d, latest %d", len(entries), seq, latest)
	}
	// Following the feed from that seq yields exactly the later change.
	s.Put(ctx, a.ID, "k3", []byte(`3`))
	later, _ := s.Changes(ctx, a.ID, seq, 100)
	if len(later) != 1 || later[0].Key != "k3" {
		t.Fatalf("changes after the list's seq = %+v", later)
	}
	// List still works and agrees with ListAt.
	plain, _ := s.List(ctx, a.ID, "k")
	if len(plain) != 3 {
		t.Fatalf("List = %+v", plain)
	}
}

// While a writer bumps a counter, every ListAt must pair the list with the
// seq of the same moment: the counter's version always equals seq - 1.
func TestListAtIsASnapshotUnderConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	tn := tenant(t, s, "a") // seq 1

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Put(ctx, tn.ID, "counter", []byte(`1`)); err != nil {
				t.Errorf("put: %v", err)
				return
			}
		}
	}()

	checked := 0
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); {
		entries, seq, err := s.ListAt(ctx, tn.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			continue
		}
		if entries[0].Version != seq-1 {
			t.Fatalf("torn snapshot: seq %d but counter at version %d", seq, entries[0].Version)
		}
		checked++
	}
	close(stop)
	wg.Wait()
	if checked < 20 {
		t.Fatalf("only %d lists checked", checked)
	}
}
