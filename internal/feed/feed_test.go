package feed

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"storecraft/internal/store"
)

func setup(t *testing.T) (*store.Store, *Feed, store.Tenant) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "feed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tn, err := st.CreateTenant(ctx, "a", "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	return st, New(st), tn
}

func TestStreamBacklogThenLiveInOrderThenStopsOnCancel(t *testing.T) {
	st, f, tn := setup(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		if _, err := st.Put(ctx, tn.ID, k, []byte("1")); err != nil {
			t.Fatal(err)
		}
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan store.Change, 16)
	var pings atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- f.Stream(sctx, tn.ID, 0, 50*time.Millisecond,
			func(cs []store.Change) error {
				for _, c := range cs {
					got <- c
				}
				return nil
			},
			func() error { pings.Add(1); return nil })
	}()

	var seqs []int64
	recv := func(n int) {
		for i := 0; i < n; i++ {
			select {
			case c := <-got:
				seqs = append(seqs, c.Seq)
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out after %d changes", len(seqs))
			}
		}
	}
	recv(3) // backlog
	for _, k := range []string{"d", "e"} {
		if _, err := st.Put(ctx, tn.ID, k, []byte("1")); err != nil {
			t.Fatal(err)
		}
	}
	recv(2) // live
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seqs = %v, want consecutive ascending", seqs)
		}
	}
	// Idle: heartbeats are sent.
	deadline := time.After(3 * time.Second)
	for pings.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d heartbeats", pings.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after ctx cancel")
	}
}

func TestPollAllWakesOnAnyTenantAndOnTenantCreation(t *testing.T) {
	st, f, a := setup(t)
	ctx := context.Background()
	b, err := st.CreateTenant(ctx, "b", "hash-b")
	if err != nil {
		t.Fatal(err)
	}
	since, _ := st.LatestSeq(ctx)

	// Timeout: empty slice, not an error.
	if got, err := f.PollAll(ctx, since, 10, 100*time.Millisecond); err != nil || len(got) != 0 {
		t.Fatalf("timeout poll = %+v, %v", got, err)
	}
	// Woken by a write of any tenant.
	type res struct {
		got []store.AdminChange
		err error
	}
	for _, trigger := range []struct {
		name string
		do   func()
		kind string
	}{
		{"tenant b write", func() { st.Put(ctx, b.ID, "k", []byte("1")) }, store.ChangeKey},
		{"tenant creation", func() { st.CreateTenant(ctx, "c", "hash-c") }, store.ChangeTenant},
		{"tenant a write", func() { st.Put(ctx, a.ID, "k", []byte("1")) }, store.ChangeKey},
	} {
		done := make(chan res, 1)
		go func() {
			g, err := f.PollAll(ctx, since, 10, 30*time.Second)
			done <- res{g, err}
		}()
		time.Sleep(100 * time.Millisecond)
		trigger.do()
		select {
		case r := <-done:
			if r.err != nil || len(r.got) != 1 || r.got[0].Kind != trigger.kind {
				t.Fatalf("%s: %+v, %v", trigger.name, r.got, r.err)
			}
			since = r.got[0].Seq
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: PollAll was not woken", trigger.name)
		}
	}
	// Cancel returns the context's error.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := f.PollAll(cctx, since, 10, 30*time.Second); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PollAll returned nil on cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollAll did not return after cancel")
	}
}

func TestStreamEndsOnWriteError(t *testing.T) {
	st, f, tn := setup(t)
	ctx := context.Background()
	st.Put(ctx, tn.ID, "a", []byte("1"))
	err := f.Stream(ctx, tn.ID, 0, time.Second,
		func([]store.Change) error { return context.DeadlineExceeded },
		func() error { return nil })
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want emit's error", err)
	}
}
