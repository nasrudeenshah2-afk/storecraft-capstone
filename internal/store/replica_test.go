package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openReplicaDB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "replica.db"), WithoutForeignKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureReplica(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func tenantChange(seq, id int64, name, hash string) AdminChange {
	return AdminChange{
		Change:   Change{Seq: seq, Key: name, Op: OpPut, Version: 1, Value: []byte(fmt.Sprintf(`{"name":%q,"token_hash":%q}`, name, hash)), TS: time.Now()},
		TenantID: id, Kind: ChangeTenant,
	}
}

func keyChange(seq, tenant int64, key, op string, version int64, value string) AdminChange {
	c := AdminChange{Change: Change{Seq: seq, Key: key, Op: op, Version: version, TS: time.Now()}, TenantID: tenant, Kind: ChangeKey}
	if op == OpPut {
		c.Value = []byte(value)
	}
	return c
}

func TestCreateTenantIsLoggedAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")
	b := tenant(t, s, "b")
	s.Put(ctx, a.ID, "k", []byte(`1`))

	all, err := s.AdminChanges(ctx, 0, 100)
	if err != nil || len(all) != 3 {
		t.Fatalf("admin changes = %d, %v", len(all), err)
	}
	if all[0].Kind != ChangeTenant || all[0].TenantID != a.ID || all[0].Key != "a" || all[0].Version != 1 ||
		string(all[0].Value) != `{"name":"a","token_hash":"hash-a"}` {
		t.Fatalf("tenant change = %+v (%s)", all[0], all[0].Value)
	}
	if all[1].TenantID != b.ID || all[2].Kind != ChangeKey || all[2].TenantID != a.ID {
		t.Fatalf("order/tenants wrong: %+v", all)
	}
	// Tenant-scoped reads never see tenant rows.
	if got, _ := s.Changes(ctx, a.ID, 0, 100); len(got) != 1 || got[0].Key != "k" {
		t.Fatalf("tenant-scoped changes = %+v", got)
	}
	if got, _ := s.Changes(ctx, b.ID, 0, 100); len(got) != 0 {
		t.Fatalf("b sees %+v", got)
	}
	if n, _ := s.LatestSeq(ctx); n != 3 {
		t.Fatalf("latest seq = %d", n)
	}

	// Duplicate: no tenant, no log row.
	if _, err := s.CreateTenant(ctx, "a", "other-hash"); !errors.Is(err, ErrTenantExists) {
		t.Fatalf("dup = %v", err)
	}
	if n, _ := s.LatestSeq(ctx); n != 3 {
		t.Fatalf("failed create burned a seq: %d", n)
	}

	// If the log insert fails, the tenant must not exist either.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_tenant BEFORE INSERT ON changes WHEN NEW.kind = 'tenant' AND NEW.key = 'ghost' BEGIN SELECT RAISE(ABORT, 'x'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTenant(ctx, "ghost", "hash-ghost"); err == nil {
		t.Fatal("CreateTenant succeeded despite a failing log insert")
	}
	if _, err := s.TenantByTokenHash(ctx, "hash-ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant row survived a failed log insert: %v", err)
	}
	if n, _ := s.LatestSeq(ctx); n != 3 {
		t.Fatalf("seq burned by rolled-back tenant creation: %d", n)
	}
}

func TestCreateTenantNotifiesListeners(t *testing.T) {
	s, _ := open(t)
	var got []int64
	s.AddCommitListener(func(id int64) { got = append(got, id) })
	a := tenant(t, s, "a")
	if len(got) != 1 || got[0] != a.ID {
		t.Fatalf("notifications = %v, want [%d]", got, a.ID)
	}
}

// Migration 4 on a database that already has data: existing rows stay 'key',
// and each pre-existing tenant gets one 'tenant' row (after the old changes).
func TestMigration4BackfillsLegacyTenants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, raw, 3); err != nil { // the schema as it was before replication
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tenants(name, token_hash) VALUES ('old-a', 'h-a'), ('old-b', 'h-b')`,
		`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (1, 'k', '"v"', 1, 1)`,
		`INSERT INTO changes(tenant_id, key, op, version, value, value_size, ts) VALUES (1, 'k', 'put', 1, '"v"', 3, 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all, err := s.AdminChanges(ctx, 0, 100)
	if err != nil || len(all) != 3 {
		t.Fatalf("after migration: %d changes, %v: %+v", len(all), err, all)
	}
	if all[0].Kind != ChangeKey || all[0].Key != "k" {
		t.Fatalf("existing change altered: %+v", all[0])
	}
	for i, name := range []string{"old-a", "old-b"} {
		c := all[i+1]
		want := fmt.Sprintf(`{"name":%q,"token_hash":%q}`, name, "h-"+name[len(name)-1:])
		if c.Kind != ChangeTenant || c.Key != name || c.TenantID != int64(i+1) || string(c.Value) != want || c.ValueSize != int64(len(want)) {
			t.Fatalf("backfilled row %d = %+v (%s)", i, c, c.Value)
		}
	}
	// The tenant-facing view still shows only the old key change.
	if got, _ := s.Changes(ctx, 1, 0, 100); len(got) != 1 || got[0].Key != "k" {
		t.Fatalf("tenant feed after migration: %+v", got)
	}
	// Re-opening does not backfill again.
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if n, _ := s2.LatestSeq(ctx); n != 3 {
		t.Fatalf("re-open changed the log: latest seq %d", n)
	}
}

func TestEnsureReplicaGuards(t *testing.T) {
	ctx := context.Background()

	// A database with data of its own can never become a replica.
	s, _ := open(t)
	tenant(t, s, "a")
	if err := s.EnsureReplica(ctx); !errors.Is(err, ErrNotReplicaDB) {
		t.Fatalf("EnsureReplica on a primary DB = %v, want ErrNotReplicaDB", err)
	}
	if isRep, _ := s.IsReplica(ctx); isRep {
		t.Fatal("primary DB reports it is a replica")
	}
	if _, err := s.AppliedSeq(ctx); !errors.Is(err, ErrNotReplicaDB) {
		t.Fatalf("AppliedSeq on a primary = %v", err)
	}
	if err := s.ApplyReplicated(ctx, tenantChange(1, 9, "x", "h")); !errors.Is(err, ErrNotReplicaDB) {
		t.Fatalf("ApplyReplicated on a primary = %v", err)
	}

	// An empty database becomes one, and EnsureReplica is idempotent.
	r := openReplicaDB(t)
	if err := r.EnsureReplica(ctx); err != nil {
		t.Fatal(err)
	}
	if isRep, _ := r.IsReplica(ctx); !isRep {
		t.Fatal("replica DB does not report it is a replica")
	}
	if n, err := r.AppliedSeq(ctx); err != nil || n != 0 {
		t.Fatalf("AppliedSeq = %d, %v", n, err)
	}
}

func TestApplyReplicatedAppliesWithPrimaryVersionsAndLocalSeqs(t *testing.T) {
	ctx := context.Background()
	r := openReplicaDB(t)

	steps := []AdminChange{
		tenantChange(1, 7, "t7", "hash-7"),
		keyChange(2, 7, "k", OpPut, 1, `"one"`),
		keyChange(3, 7, "k", OpPut, 5, `"five"`), // the primary's version is used as-is
		keyChange(4, 7, "gone", OpDelete, 2, ""), // delete of a key this replica never had: no effect
		keyChange(5, 7, "k", OpDelete, 5, ""),
		keyChange(6, 7, "j", OpPut, 1, `{"x":1}`),
	}
	for _, c := range steps {
		if err := r.ApplyReplicated(ctx, c); err != nil {
			t.Fatalf("apply seq %d: %v", c.Seq, err)
		}
	}
	if n, _ := r.AppliedSeq(ctx); n != 6 {
		t.Fatalf("applied_seq = %d, want 6 (the primary's seq)", n)
	}
	// Same tenant id and token as on the primary.
	if tn, err := r.TenantByTokenHash(ctx, "hash-7"); err != nil || tn.ID != 7 || tn.Name != "t7" {
		t.Fatalf("tenant = %+v, %v", tn, err)
	}
	if _, err := r.Get(ctx, 7, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("k should be deleted: %v", err)
	}
	if e, err := r.Get(ctx, 7, "j"); err != nil || e.Version != 1 || string(e.Value) != `{"x":1}` {
		t.Fatalf("j = %+v, %v", e, err)
	}

	// The replica logs its OWN changes with its OWN seqs: the no-op delete (seq 4)
	// has no local row, so local seqs run 1..5 while applied_seq is 6.
	local, err := r.AdminChanges(ctx, 0, 100)
	if err != nil || len(local) != 5 {
		t.Fatalf("local changes = %d, %v: %+v", len(local), err, local)
	}
	for i, c := range local {
		if c.Seq != int64(i+1) {
			t.Fatalf("local seq %d at index %d", c.Seq, i)
		}
	}
	if latest, _ := r.LatestSeq(ctx); latest != 5 {
		t.Fatalf("local latest seq = %d, want 5 (not the primary's 6)", latest)
	}
	if local[2].Version != 5 || string(local[2].Value) != `"five"` {
		t.Fatalf("local change kept the wrong version: %+v", local[2])
	}
	// Tenant-facing view on the replica.
	if got, _ := r.Changes(ctx, 7, 0, 100); len(got) != 4 {
		t.Fatalf("tenant changes on replica = %d, want 4", len(got))
	}
}

func TestApplyReplicatedIsIdempotentAndRejectsGaps(t *testing.T) {
	ctx := context.Background()
	r := openReplicaDB(t)
	r.ApplyReplicated(ctx, tenantChange(1, 1, "t", "h"))
	r.ApplyReplicated(ctx, keyChange(2, 1, "k", OpPut, 1, `"a"`))
	r.ApplyReplicated(ctx, keyChange(3, 1, "k", OpPut, 2, `"b"`))
	before, _ := r.LatestSeq(ctx)

	// Re-delivery of anything already applied is a no-op.
	for _, c := range []AdminChange{tenantChange(1, 1, "t", "h"), keyChange(2, 1, "k", OpPut, 1, `"a"`), keyChange(3, 1, "k", OpPut, 2, `"b"`)} {
		if err := r.ApplyReplicated(ctx, c); err != nil {
			t.Fatalf("redelivery of seq %d: %v", c.Seq, err)
		}
	}
	if e, _ := r.Get(ctx, 1, "k"); e.Version != 2 || string(e.Value) != `"b"` {
		t.Fatalf("redelivery changed data: %+v", e)
	}
	if after, _ := r.LatestSeq(ctx); after != before {
		t.Fatalf("redelivery wrote local change rows: %d -> %d", before, after)
	}
	if n, _ := r.AppliedSeq(ctx); n != 3 {
		t.Fatalf("applied_seq = %d", n)
	}

	// A gap is refused and applies nothing.
	err := r.ApplyReplicated(ctx, keyChange(5, 1, "later", OpPut, 1, `"x"`))
	if !errors.Is(err, ErrReplicationGap) {
		t.Fatalf("gap err = %v, want ErrReplicationGap", err)
	}
	if _, err := r.Get(ctx, 1, "later"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a change after a gap was applied")
	}
	if n, _ := r.AppliedSeq(ctx); n != 3 {
		t.Fatalf("applied_seq moved on a gap: %d", n)
	}
}

// The applied change and applied_seq are one transaction: if either part fails neither is saved.
func TestApplyReplicatedIsAtomicWithAppliedSeq(t *testing.T) {
	ctx := context.Background()
	r := openReplicaDB(t)
	r.ApplyReplicated(ctx, tenantChange(1, 1, "t", "h"))

	// Fail the local change-log insert: kv write and applied_seq must roll back.
	if _, err := r.db.Exec(`CREATE TRIGGER fail_local BEFORE INSERT ON changes WHEN NEW.key = 'boom' BEGIN SELECT RAISE(ABORT, 'x'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyReplicated(ctx, keyChange(2, 1, "boom", OpPut, 1, `"x"`)); err == nil {
		t.Fatal("apply succeeded despite a failing insert")
	}
	if _, err := r.Get(ctx, 1, "boom"); !errors.Is(err, ErrNotFound) {
		t.Fatal("kv write survived a failed apply")
	}
	if n, _ := r.AppliedSeq(ctx); n != 1 {
		t.Fatalf("applied_seq advanced to %d despite the failure", n)
	}

	// Fail the applied_seq update instead: the data write must roll back.
	if _, err := r.db.Exec(`DROP TRIGGER fail_local`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`CREATE TRIGGER fail_state BEFORE UPDATE ON replica_state BEGIN SELECT RAISE(ABORT, 'x'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyReplicated(ctx, keyChange(2, 1, "k", OpPut, 1, `"x"`)); err == nil {
		t.Fatal("apply succeeded despite a failing applied_seq update")
	}
	if _, err := r.Get(ctx, 1, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal("kv write survived a failed applied_seq update")
	}
	if local, _ := r.AdminChanges(ctx, 0, 100); len(local) != 1 {
		t.Fatalf("local change row survived: %d rows", len(local))
	}

	// Fault removed: the same change applies normally.
	if _, err := r.db.Exec(`DROP TRIGGER fail_state`); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyReplicated(ctx, keyChange(2, 1, "k", OpPut, 1, `"x"`)); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.AppliedSeq(ctx); n != 2 {
		t.Fatalf("applied_seq = %d, want 2", n)
	}
}
