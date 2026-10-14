package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"storecraft/internal/api"
	"storecraft/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func cfg(role, db, primary, token string) nodeConfig {
	return nodeConfig{Role: role, DB: db, PrimaryURL: primary, ReplicationToken: token, Log: quiet}
}

func TestOpenNodeValidatesFlags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for name, c := range map[string]struct {
		cfg  nodeConfig
		want string
	}{
		"bad role":                  {cfg("leader", filepath.Join(dir, "a.db"), "", ""), "-role must be"},
		"replica without -primary":  {cfg(api.RoleReplica, filepath.Join(dir, "b.db"), "", "tok"), "-primary is required"},
		"replica without token":     {cfg(api.RoleReplica, filepath.Join(dir, "c.db"), "http://localhost:1", ""), "STORECRAFT_REPLICATION_TOKEN"},
		"replica with a bad URL":    {cfg(api.RoleReplica, filepath.Join(dir, "d.db"), "localhost:8080", "tok"), "http(s) URL"},
		"primary with -primary set": {cfg(api.RolePrimary, filepath.Join(dir, "e.db"), "http://localhost:1", ""), "only valid with -role=replica"},
	} {
		st, rep, err := openNode(ctx, c.cfg)
		if err == nil {
			st.Close()
			t.Errorf("%s: openNode succeeded, want an error containing %q", name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, c.want)
		}
		if st != nil || rep != nil {
			t.Errorf("%s: returned a store/replica together with an error", name)
		}
	}
}

// Bad flags must fail before touching the database file.
func TestOpenNodeDoesNotCreateDatabaseOnBadFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never.db")
	if _, _, err := openNode(context.Background(), cfg(api.RoleReplica, path, "", "tok")); err == nil {
		t.Fatal("expected an error")
	}
	if matches, _ := filepath.Glob(path + "*"); len(matches) != 0 {
		t.Fatalf("database files created despite invalid flags: %v", matches)
	}
}

// A primary database can never be started as a replica, nor a replica database as a primary.
func TestOpenNodeRoleSafety(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A database with data of its own (here: a tenant) is refused as a replica.
	primaryDB := filepath.Join(dir, "primary.db")
	st, rep, err := openNode(ctx, cfg(api.RolePrimary, primaryDB, "", ""))
	if err != nil || rep != nil {
		t.Fatalf("primary on a fresh DB: %v (replica %v)", err, rep)
	}
	if _, err := st.CreateTenant(ctx, "acme", "h"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, _, err := openNode(ctx, cfg(api.RoleReplica, primaryDB, "http://localhost:1", "tok")); err == nil {
		st.Close()
		t.Fatal("a primary database was accepted as a replica database")
	} else if !strings.Contains(err.Error(), "cannot use") {
		t.Fatalf("unhelpful error: %v", err)
	}
	// The refused attempt must not have marked it as a replica.
	st, _, err = openNode(ctx, cfg(api.RolePrimary, primaryDB, "", ""))
	if err != nil {
		t.Fatalf("primary DB no longer opens as a primary: %v", err)
	}
	st.Close()

	// A fresh database becomes a replica, reopens as one, and then refuses the primary role.
	replicaDB := filepath.Join(dir, "replica.db")
	for i := 0; i < 2; i++ {
		st, rep, err := openNode(ctx, cfg(api.RoleReplica, replicaDB, "http://localhost:1", "tok"))
		if err != nil || rep == nil {
			t.Fatalf("replica open #%d: %v (replica %v)", i, err, rep)
		}
		st.Close()
	}
	if st, _, err := openNode(ctx, cfg(api.RolePrimary, replicaDB, "", "")); err == nil {
		st.Close()
		t.Fatal("a replica database was accepted as a primary")
	} else if !strings.Contains(err.Error(), "is a replica database") {
		t.Fatalf("unhelpful error: %v", err)
	}

	// `seed` also refuses a replica database.
	if err := seed([]string{"-db", replicaDB}, io.Discard); err == nil || !strings.Contains(err.Error(), "replica database") {
		t.Fatalf("seed on a replica DB = %v, want a refusal", err)
	}
	// And still works on a primary DB.
	seedDB := filepath.Join(dir, "seeded.db")
	if err := seed([]string{"-db", seedDB}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, seedDB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, _ := s.LatestSeq(ctx); n != 3 { // three tenant-creation rows
		t.Fatalf("seed logged %d changes, want 3", n)
	}
}
