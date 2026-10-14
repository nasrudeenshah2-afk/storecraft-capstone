// Package feed serves the change log, including long-polling for new changes.
package feed

import (
	"context"
	"sync"
	"time"

	"storecraft/internal/store"
)

// Feed wakes long-pollers when their tenant's data changes.
type Feed struct {
	st *store.Store

	mu    sync.Mutex
	waits map[int64]chan struct{} // closed and replaced on every commit for the tenant
	all   chan struct{}           // closed and replaced on every commit for any tenant
}

// New creates a Feed and subscribes it to commits on st.
func New(st *store.Store) *Feed {
	f := &Feed{st: st, waits: map[int64]chan struct{}{}}
	st.AddCommitListener(f.wake)
	return f
}

func (f *Feed) wake(tenantID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.waits[tenantID]; ok {
		close(ch)
		delete(f.waits, tenantID)
	}
	if f.all != nil {
		close(f.all)
		f.all = nil
	}
}

// waitAll returns a channel that is closed on the next commit for any tenant.
func (f *Feed) waitAll() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.all == nil {
		f.all = make(chan struct{})
	}
	return f.all
}

// PollAll is the admin (all tenants, all kinds) counterpart of Poll: it returns
// up to limit changes with seq > since, waiting up to timeout if there are
// none (then it returns an empty slice), or until ctx is done (ctx's error).
func (f *Feed) PollAll(ctx context.Context, since int64, limit int, timeout time.Duration) ([]store.AdminChange, error) {
	return f.PollAllUntil(ctx, since, limit, timeout, nil)
}

// PollAllUntil is PollAll that also returns (with no changes) as soon as done
// reports true. done is checked after every commit wake-up, so it can watch
// state that changes without a change row (the admin feed uses it to answer a
// replica whose copy of a flag is out of date).
func (f *Feed) PollAllUntil(ctx context.Context, since int64, limit int, timeout time.Duration,
	done func(context.Context) (bool, error)) ([]store.AdminChange, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		wake := f.waitAll() // subscribe before querying: no lost wake-ups
		changes, err := f.st.AdminChanges(ctx, since, limit)
		if err != nil {
			return nil, err
		}
		if len(changes) > 0 {
			return changes, nil
		}
		if done != nil {
			stop, err := done(ctx)
			if err != nil {
				return nil, err
			}
			if stop {
				return []store.AdminChange{}, nil
			}
		}
		select {
		case <-wake:
		case <-timer.C:
			return []store.AdminChange{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitChan returns a channel that is closed on the tenant's next commit.
func (f *Feed) waitChan(tenantID int64) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.waits[tenantID]
	if !ok {
		ch = make(chan struct{})
		f.waits[tenantID] = ch
	}
	return ch
}

const streamBatch = 100

// Stream delivers the tenant's changes with seq > since to emit, in ascending
// order, then keeps delivering new ones as they commit. When idle for a full
// heartbeat interval it calls ping. It returns nil when ctx is done, or the
// first error from the store, emit or ping (for example a failed client write).
func (f *Feed) Stream(ctx context.Context, tenantID, since int64, heartbeat time.Duration,
	emit func([]store.Change) error, ping func() error) error {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		// Subscribe before querying so a commit between the query and the
		// wait cannot be missed.
		wake := f.waitChan(tenantID)
		changes, err := f.st.Changes(ctx, tenantID, since, streamBatch)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(changes) > 0 {
			if err := emit(changes); err != nil {
				return err
			}
			since = changes[len(changes)-1].Seq
			continue // drain any backlog before waiting
		}
		select {
		case <-wake:
		case <-ticker.C:
			if err := ping(); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// Poll returns up to limit changes with seq > since for the tenant. If there
// are none it waits until one is committed, timeout elapses (returning an
// empty slice), or ctx is done (returning ctx's error).
func (f *Feed) Poll(ctx context.Context, tenantID, since int64, limit int, timeout time.Duration) ([]store.Change, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		// Subscribe before querying so a commit between the query and the
		// wait cannot be missed.
		wake := f.waitChan(tenantID)
		changes, err := f.st.Changes(ctx, tenantID, since, limit)
		if err != nil {
			return nil, err
		}
		if len(changes) > 0 {
			return changes, nil
		}
		select {
		case <-wake:
		case <-timer.C:
			return []store.Change{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
