package verify_test

// Regression tests for findings of the independent review (B13 triage,
// docs/capstone/review-triage.md).

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"storecraft/internal/clustertest"
	"storecraft/internal/verify"
)

// R3: stopping verify (Ctrl+C cancels its context) is not a timeout. The old
// message "timed out after 1m0s: replica applied_seq ... has not reached"
// pointed the operator at replica lag that was never measured.
func TestReviewR3InterruptIsNotReportedAsTimeout(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.Pause() // lagging: verify will wait
	clustertest.Put(t, p.URL, tok, "k", `1`)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel) // the operator presses Ctrl+C
	c := cfg(p, r)
	c.Timeout = time.Minute
	start := time.Now()
	res, err := verify.Run(ctx, c)
	if res != nil || err == nil {
		t.Fatalf("interrupted run returned %+v, %v", res, err)
	}
	if errors.Is(err, verify.ErrTimeout) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("an interrupt was reported as a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("error %q does not say it was interrupted", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("verify did not stop promptly after the interrupt")
	}
	if verify.ExitCode(res, err) != 2 {
		t.Fatal("an interrupt must still exit 2 (no verdict)")
	}
}

// R6: a replica whose copy of the flag has a HIGHER version than the
// primary's (for example after the primary was restored from a backup) must
// still converge to the primary's state, and must not poll in a hot loop.
func TestReviewR6ReplicaFlagAheadOfPrimaryConverges(t *testing.T) {
	p := clustertest.StartPrimary(t)
	tok := p.AddTenant("acme")
	r := clustertest.StartReplica(t, p.URL, filepath.Join(t.TempDir(), "replica.db"))
	r.WaitSynced(p)
	r.Pause()
	db := rawDB(t, r.DBPath)
	exec(t, db, `INSERT INTO flags(name, enabled, version, updated_at) VALUES ('quota_enforcement', 1, 9, 1)`)
	before := len(p.SinceLog())
	r.Resume()

	clustertest.WaitFor(t, 5*time.Second, "the replica's flag to equal the primary's", func() bool {
		pf := clustertest.Call(t, "GET", p.URL+"/v1/flags", tok, "", nil)
		rf := clustertest.Call(t, "GET", r.URL+"/v1/flags", tok, "", nil)
		return rf.Status == 200 && rf.Body == pf.Body
	})
	time.Sleep(time.Second)
	n := len(p.SinceLog()) - before
	// Idle long-polls last 300 ms in this cluster, so about 3-6 requests in a
	// bit over a second is normal; a hot loop makes several per 250 ms pause.
	if n > 12 {
		t.Fatalf("the replica made %d feed requests in about a second: hot loop", n)
	}
}
