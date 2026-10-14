package main

// Regression test for a finding of the independent review (B13 triage).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"storecraft/internal/store"
)

// R9: backfill-usage on a REPLICA database rewrites kv outside
// store.ApplyReplicated (only the spelling of values and the counters; never a
// version or a JSON value). That is a deliberate, documented exception, so the
// command must say so instead of doing it silently.
func TestReviewR9BackfillOnReplicaSaysWhatItDoes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "replica.db")
	st, err := store.Open(context.Background(), dbPath, store.WithoutForeignKeys())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureReplica(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	code, out := runBin(t, currentBinary(t), "backfill-usage", "-db", dbPath)
	if code != 0 || !strings.Contains(out, "replica database") || !strings.Contains(out, "versions") {
		t.Fatalf("backfill-usage on a replica: exit %d, output:\n%s\nwant a note that this is a replica database and versions are not changed", code, out)
	}
}
