package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func tenant(t *testing.T, s *Store, name string) Tenant {
	t.Helper()
	tn, err := s.CreateTenant(context.Background(), name, "hash-"+name)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func TestMigrationsAndWAL(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)

	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}

	// Reopen: migrations must not be reapplied.
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n, max int
	if err := s2.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(version) FROM schema_migrations`).Scan(&n, &max); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) || max != len(migrations) || max != 6 {
		t.Fatalf("schema_migrations count=%d max=%d, want %d/%d (6 migrations: STORE-210 added 6)", n, max, len(migrations), len(migrations))
	}
}

func TestTenants(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")
	got, err := s.TenantByTokenHash(ctx, "hash-a")
	if err != nil || got.ID != a.ID || got.Name != "a" {
		t.Fatalf("TenantByTokenHash = %+v, %v", got, err)
	}
	if _, err := s.TenantByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("miss err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateTenant(ctx, "a", "other"); !errors.Is(err, ErrTenantExists) {
		t.Fatalf("dup name err = %v, want ErrTenantExists", err)
	}
	if _, err := s.CreateTenant(ctx, "b", "hash-a"); !errors.Is(err, ErrTenantExists) {
		t.Fatalf("dup hash err = %v, want ErrTenantExists", err)
	}
}

func TestCRUDAndVersions(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	if _, err := s.Get(ctx, a.ID, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	for want := int64(1); want <= 3; want++ {
		v, err := s.Put(ctx, a.ID, "k", []byte(fmt.Sprintf("v%d", want)))
		if err != nil || v != want {
			t.Fatalf("Put version = %d, %v; want %d", v, err, want)
		}
	}
	e, err := s.Get(ctx, a.ID, "k")
	if err != nil || string(e.Value) != "v3" || e.Version != 3 || e.UpdatedAt.IsZero() {
		t.Fatalf("Get = %+v, %v", e, err)
	}
	if err := s.Delete(ctx, a.ID, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, a.ID, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	// Re-creating after delete restarts at version 1.
	if v, _ := s.Put(ctx, a.ID, "k", nil); v != 1 {
		t.Fatalf("version after recreate = %d, want 1", v)
	}
}

func TestListPrefix(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")
	for _, k := range []string{"app/b", "app/a", "apple", "other", "100%", "100x"} {
		if _, err := s.Put(ctx, a.ID, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(prefix string) []string {
		es, err := s.List(ctx, a.ID, prefix)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Key)
		}
		return out
	}
	if got := fmt.Sprint(keys("app/")); got != "[app/a app/b]" {
		t.Fatalf("app/ = %s", got)
	}
	if got := len(keys("")); got != 6 {
		t.Fatalf("empty prefix returned %d, want 6", got)
	}
	// LIKE wildcards must be literal.
	if got := fmt.Sprint(keys("100%")); got != "[100%]" {
		t.Fatalf("100%% = %s", got)
	}
	if got := keys("zzz"); len(got) != 0 {
		t.Fatalf("zzz = %v", got)
	}
}

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := tenant(t, s, "a"), tenant(t, s, "b")

	if _, err := s.Put(ctx, a.ID, "same", []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, b.ID, "same"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("b read a's key: %v", err)
	}
	if v, err := s.Put(ctx, b.ID, "same", []byte("from-b")); err != nil || v != 1 {
		t.Fatalf("b put version = %d, %v; want independent version 1", v, err)
	}
	ea, _ := s.Get(ctx, a.ID, "same")
	eb, _ := s.Get(ctx, b.ID, "same")
	if string(ea.Value) != "from-a" || string(eb.Value) != "from-b" {
		t.Fatalf("values crossed: a=%q b=%q", ea.Value, eb.Value)
	}
	if es, _ := s.List(ctx, a.ID, ""); len(es) != 1 || string(es[0].Value) != "from-a" {
		t.Fatalf("a list = %+v", es)
	}
	// Deleting b's key must not touch a's.
	if err := s.Delete(ctx, b.ID, "same"); err != nil {
		t.Fatal(err)
	}
	if e, err := s.Get(ctx, a.ID, "same"); err != nil || string(e.Value) != "from-a" {
		t.Fatalf("a's key affected by b's delete: %+v, %v", e, err)
	}
}

func TestConcurrentPutVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a := tenant(t, s, "a")

	const n = 50
	versions := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			versions[i], errs[i] = s.Put(ctx, a.ID, "counter", []byte(fmt.Sprint(i)))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for i, v := range versions {
		if v != int64(i+1) {
			t.Fatalf("returned versions not exactly 1..%d: %v", n, versions)
		}
	}
	e, err := s.Get(ctx, a.ID, "counter")
	if err != nil || e.Version != n {
		t.Fatalf("final version = %d, %v; want %d", e.Version, err, n)
	}
}
