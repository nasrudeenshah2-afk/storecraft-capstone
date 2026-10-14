package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bs makes JSON escapes readable in Go source: every X becomes a backslash.
func bs(s string) string { return strings.ReplaceAll(s, "X", `\`) }

// usageVsData returns the stored counters and COUNT/SUM over kv, per tenant,
// as comparable strings (tenants with neither are left out).
func usageVsData(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (stored, real string) {
	t.Helper()
	q := func(query string) string {
		rows, err := db.QueryContext(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, k, n int64
			if err := rows.Scan(&id, &k, &n); err != nil {
				t.Fatal(err)
			}
			if k != 0 || n != 0 {
				fmt.Fprintf(&b, "%d:%d/%d ", id, k, n)
			}
		}
		return b.String()
	}
	return q(`SELECT tenant_id, key_count, byte_count FROM tenant_usage ORDER BY 1`),
		q(`SELECT tenant_id, COUNT(*), SUM(length(CAST(value AS BLOB))) FROM kv GROUP BY tenant_id ORDER BY 1`)
}

func requireUsageExact(t *testing.T, s *Store, when string) {
	t.Helper()
	stored, real := usageVsData(t, s.db)
	if stored != real {
		t.Fatalf("%s: tenant_usage %q, data %q", when, stored, real)
	}
}

func TestQuotaCanonicalJSON(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{ "b" : 1 ,  "a" : [ 1 , 2.50 , 1e3, -0, 1E-7 ] }`, `{"b":1,"a":[1,2.50,1e3,-0,1E-7]}`},
		{`"<b>Tom & Jerry</b>"`, `"<b>Tom & Jerry</b>"`},
		{bs(`"Xu003cXu0026Xu003e Xu00e9 Xn Xt X" XX X/"`), "\"<&> é \\n \\t \\\" \\\\ /\""},
		{bs(`"Xu2028"`), bs(`"Xu2028"`)},
		{` [ true , false , null , {} , [] , "" ] `, `[true,false,null,{},[],""]`},
		{`{"dup":1,"dup":2}`, `{"dup":1,"dup":2}`},
		{`12345678901234567890123`, `12345678901234567890123`},
	} {
		if got := string(CanonicalJSON([]byte(c.in))); got != c.want {
			t.Errorf("CanonicalJSON(%s) = %s, want %s", c.in, got, c.want)
		}
	}
	for _, notJSON := range []string{`plain text`, `{"a":`, `1 2`, ``} {
		if got := string(CanonicalJSON([]byte(notJSON))); got != notJSON {
			t.Errorf("not JSON %q came back as %q, want unchanged", notJSON, got)
		}
	}
	// Canonical form is a fixed point.
	in := []byte(bs(`{"x": [ "Xu0041", {"y" : null} ] }`))
	once := CanonicalJSON(in)
	if twice := CanonicalJSON(once); string(twice) != string(once) || string(once) != `{"x":["A",{"y":null}]}` {
		t.Fatalf("once %s, twice %s", once, twice)
	}
}

// Migration 6 on a database that already has data: nothing existing changes,
// the new tables exist, and usage starts equal to the data.
func TestQuotaMigration6IsAdditive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, raw, 5); err != nil { // the v1.0.0 schema
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tenants(name, token_hash) VALUES ('a', 'h-a'), ('b', 'h-b'), ('empty', 'h-e')`,
		`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (1, 'k1', '{ "spaced": true }', 3, 1), (1, 'k2', '"é"', 1, 1), (2, 'k', '[1,2]', 1, 1)`,
		`INSERT INTO changes(tenant_id, key, op, version, value, value_size, ts) VALUES (1, 'k1', 'put', 3, '{ "spaced": true }', 18, 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	dump := func(db *sql.DB) string {
		rows, err := db.Query(`SELECT tenant_id, key, CAST(value AS TEXT), version FROM kv ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, v int64
			var k, val string
			rows.Scan(&id, &k, &val, &v)
			fmt.Fprintf(&b, "%d %s %s %d\n", id, k, val, v)
		}
		return b.String()
	}
	before := dump(raw)
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if after := dump(s.db); after != before {
		t.Fatalf("migration changed existing data:\n%s\n%s", before, after)
	}
	var subject sql.NullString
	if err := s.db.QueryRow(`SELECT subject FROM changes WHERE kind = 'key'`).Scan(&subject); err != nil || subject.Valid {
		t.Fatalf("old change row subject = %v, %v; want NULL", subject, err)
	}
	for _, table := range []string{"tenant_quotas", "flags", "backfill_progress"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d rows, %v; want an empty table", table, n, err)
		}
	}
	requireUsageExact(t, s, "right after migration 6")
	if stored, _ := usageVsData(t, s.db); stored != "1:2/22 2:1/5 " {
		t.Fatalf("initial usage %q, want 1:2/22 2:1/5 (raw bytes 18 + 4, é is 2 bytes; 5)", stored)
	}
}

// The triggers keep usage exact for every kind of write, including writes
// that bypass this build's code (as the v1.0.0 binary's would).
func TestQuotaUsageTriggersFollowEveryWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := tenant(t, s, "a"), tenant(t, s, "b")
	steps := []struct {
		what string
		do   func() error
	}{
		{"put new", func() error { _, err := s.Put(ctx, a.ID, "k1", []byte(`"one"`)); return err }},
		{"put overwrite larger", func() error { _, err := s.Put(ctx, a.ID, "k1", []byte(`"one two three"`)); return err }},
		{"put-if-version smaller", func() error { _, err := s.PutIfVersion(ctx, a.ID, "k1", []byte(`1`), 2); return err }},
		{"put-if-absent", func() error { _, err := s.PutIfAbsent(ctx, b.ID, "k", []byte(`[1,2,3]`)); return err }},
		{"failed put-if-version", func() error { s.PutIfVersion(ctx, a.ID, "k1", []byte(`"x"`), 99); return nil }},
		{"delete", func() error { return s.Delete(ctx, a.ID, "k1") }},
		{"raw insert", func() error {
			_, err := s.wdb.Exec(`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, 'raw', '{ "a" : 1 }', 1, 1)`, b.ID)
			return err
		}},
		{"raw upsert", func() error {
			_, err := s.wdb.Exec(`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, 'raw', '"short"', 1, 1)
				ON CONFLICT(tenant_id, key) DO UPDATE SET value = excluded.value, version = kv.version + 1`, b.ID)
			return err
		}},
		{"raw delete", func() error {
			_, err := s.wdb.Exec(`DELETE FROM kv WHERE tenant_id = ? AND key = 'k'`, b.ID)
			return err
		}},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.what, err)
		}
		requireUsageExact(t, s, st.what)
	}
}

// AC-API-13 at the store level: SET (never add), twice = no change, resume.
func TestQuotaBackfillUsage(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	for i := 1; i <= 3; i++ {
		tn := tenant(t, s, fmt.Sprintf("t%d", i))
		for k := 0; k < 3; k++ {
			if _, err := s.wdb.Exec(`INSERT INTO kv(tenant_id, key, value, version, updated_at) VALUES (?, ?, ?, 1, 1)`,
				tn.ID, fmt.Sprintf("k%d", k), bs(fmt.Sprintf(`{ "n" : %d, "s" : "Xu003c&" }`, k))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Break the counters three ways: inflated, missing, and a stale row for a
	// tenant that has no keys.
	for _, q := range []string{
		`UPDATE tenant_usage SET key_count = 50, byte_count = 1 WHERE tenant_id = 1`,
		`DELETE FROM tenant_usage WHERE tenant_id = 2`,
		`INSERT INTO tenant_usage(tenant_id, key_count, byte_count) VALUES (77, 4, 40)`,
	} {
		if _, err := s.wdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.BackfillUsage(ctx, BackfillOptions{Limit: 2})
	if err != nil || res.Processed != 2 || res.Remaining != 2 {
		t.Fatalf("limited run: %+v, %v", res, err)
	}
	res, err = s.BackfillUsage(ctx, BackfillOptions{})
	if err != nil || res.Processed != 2 || res.AlreadyDone != 2 || res.Remaining != 0 {
		t.Fatalf("resumed run: %+v, %v", res, err)
	}
	requireUsageExact(t, s, "after backfill")
	var v string
	if err := s.db.QueryRow(`SELECT CAST(value AS TEXT) FROM kv WHERE tenant_id = 3 AND key = 'k2'`).Scan(&v); err != nil || v != `{"n":2,"s":"<&"}` {
		t.Fatalf("value after backfill %q, %v", v, err)
	}
	if stored, _ := usageVsData(t, s.db); strings.Contains(stored, "77:") {
		t.Fatalf("stale usage row for a tenant without keys survived: %q", stored)
	}
	var progress int
	s.db.QueryRow(`SELECT COUNT(*) FROM backfill_progress`).Scan(&progress)
	if progress != 0 {
		t.Fatalf("progress rows after a complete run = %d, want 0", progress)
	}

	stored1, _ := usageVsData(t, s.db)
	res, err = s.BackfillUsage(ctx, BackfillOptions{})
	stored2, _ := usageVsData(t, s.db)
	if err != nil || res.Rewritten != 0 || stored1 != stored2 {
		t.Fatalf("second complete run: %+v, %v; usage %q -> %q", res, err, stored1, stored2)
	}
}

// Review finding R2 (B13): backfill-usage must not hold the write lock for long.
// A tenant with many legacy values used to be canonicalised in ONE transaction
// (200k values: 36 s), so the server's writes waited past busy_timeout (5 s)
// and failed with 500. A server is simulated by a second Store on the same
// file (its own write connection, as another process has).
func TestReviewR2BackfillKeepsServerWritesFast(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	big := tenant(t, s, "big")
	rows := 20000
	if raceEnabled {
		rows = 6000 // the race detector makes every write about 5x slower
	}
	if _, err := s.wdb.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO kv(tenant_id, key, value, version, updated_at) SELECT ?, 'k'||i, '{ "n" : '||i||' , "s" : "legacy text" }', 1, 1 FROM n`, rows, big.ID); err != nil {
		t.Fatal(err)
	}
	server, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	other := tenant(t, server, "live")

	done := make(chan error, 1)
	go func() { _, err := s.BackfillUsage(ctx, BackfillOptions{}); done <- err }()
	var worst time.Duration
	writes := 0
	for finished := false; !finished; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			finished = true
		default:
			start := time.Now()
			if _, err := server.Put(ctx, other.ID, fmt.Sprintf("w%d", writes%50), []byte(`1`)); err != nil {
				t.Fatalf("a server write failed during the backfill: %v", err)
			}
			worst = max(worst, time.Since(start))
			writes++
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Logf("%d server writes during the backfill; slowest %v", writes, worst)
	// The requirement is that no server write fails (busy_timeout is 5 s); the
	// margin asked for is half of that. (A first threshold of 1.5 s was an
	// arbitrary choice and was hit at 1.62 s under the full parallel CI load,
	// with no failed write; see review-triage.md R2.)
	if worst > 2500*time.Millisecond {
		t.Fatalf("a server write waited %v for the backfill (it must stay under half of busy_timeout, 2.5 s)", worst)
	}
	requireUsageExact(t, s, "after a backfill during writes")
	var v string
	if err := s.db.QueryRow(`SELECT CAST(value AS TEXT) FROM kv WHERE tenant_id = ? AND key = ?`, big.ID, fmt.Sprintf("k%d", rows)).Scan(&v); err != nil || v != fmt.Sprintf(`{"n":%d,"s":"legacy text"}`, rows) {
		t.Fatalf("last value after backfill %q, %v", v, err)
	}
}
