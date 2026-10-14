package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type rec struct {
	key     string
	version int64
	value   string
}

// expectedChecksum re-implements the documented checksum format independently:
// SHA-256 over records sorted by key, each uvarint(len(key)) key | version
// (8 bytes big-endian) | SHA-256(value).
func expectedChecksum(recs []rec) string {
	h := sha256.New()
	for _, r := range recs { // callers pass them sorted
		var lb [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(lb[:], uint64(len(r.key)))
		h.Write(lb[:n])
		h.Write([]byte(r.key))
		var vb [8]byte
		binary.BigEndian.PutUint64(vb[:], uint64(r.version))
		h.Write(vb[:])
		sum := sha256.Sum256([]byte(r.value))
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestOf(t *testing.T, s *Store, tenantID int64, entries bool) Digest {
	t.Helper()
	d, err := s.Digest(context.Background(), tenantID, entries)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func byID(d Digest, id int64) TenantDigest {
	for _, t := range d.Tenants {
		if t.TenantID == id {
			return t
		}
	}
	return TenantDigest{TenantID: -1}
}

func TestDigestCountsBytesAndDocumentedChecksum(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b, empty := tenant(t, s, "a"), tenant(t, s, "b"), tenant(t, s, "empty")

	s.Put(ctx, a.ID, "zeta", []byte(`"last"`))    // 6 bytes
	s.Put(ctx, a.ID, "alpha", []byte(`{"x": 1}`)) // stored with a space; canonical form is 7 bytes
	s.Put(ctx, a.ID, "alpha", []byte(`{"x": 2}`)) // version 2
	s.Put(ctx, a.ID, "mid", []byte(`not json`))   // invalid JSON is hashed as stored (8 bytes)
	s.Put(ctx, b.ID, "only", []byte(`1`))

	d := digestOf(t, s, 0, false)
	ta, tb, te := byID(d, a.ID), byID(d, b.ID), byID(d, empty.ID)
	if ta.Name != "a" || ta.Keys != 3 || ta.ValueBytes != 7+8+6 {
		t.Fatalf("tenant a = %+v", ta)
	}
	want := expectedChecksum([]rec{
		{"alpha", 2, `{"x":2}`}, {"mid", 1, `not json`}, {"zeta", 1, `"last"`},
	})
	if ta.Checksum != want {
		t.Fatalf("checksum = %s, want %s (documented format)", ta.Checksum, want)
	}
	if tb.Keys != 1 || tb.ValueBytes != 1 || tb.Checksum != expectedChecksum([]rec{{"only", 1, "1"}}) {
		t.Fatalf("tenant b = %+v", tb)
	}
	if te.Keys != 0 || te.ValueBytes != 0 || te.Checksum != expectedChecksum(nil) {
		t.Fatalf("empty tenant = %+v", te)
	}
	if len(d.Tenants) != 3 || d.Tenants[0].TenantID > d.Tenants[1].TenantID {
		t.Fatalf("tenants not listed by id: %+v", d.Tenants)
	}
	// Seq: 3 tenant rows + 5 key writes on a primary.
	if d.Seq != 8 {
		t.Fatalf("seq = %d, want 8", d.Seq)
	}
	if len(ta.Entries) != 0 {
		t.Fatal("entries returned without being requested")
	}
}

func TestDigestChecksumChangesWithKeyVersionValueAndIgnoresInsertOrderAndWhitespace(t *testing.T) {
	ctx := context.Background()
	checksumFor := func(build func(s *Store, id int64)) string {
		s, _ := open(t)
		tn := tenant(t, s, "x")
		build(s, tn.ID)
		return byID(digestOf(t, s, 0, false), tn.ID).Checksum
	}
	base := checksumFor(func(s *Store, id int64) {
		s.Put(ctx, id, "a", []byte(`1`))
		s.Put(ctx, id, "b", []byte(`2`))
	})
	same := map[string]func(s *Store, id int64){
		"other insert order": func(s *Store, id int64) { s.Put(ctx, id, "b", []byte(`2`)); s.Put(ctx, id, "a", []byte(`1`)) },
		"whitespace":         func(s *Store, id int64) { s.Put(ctx, id, "a", []byte(` 1 `)); s.Put(ctx, id, "b", []byte("2\n")) },
	}
	for name, f := range same {
		if got := checksumFor(f); got != base {
			t.Errorf("%s changed the checksum", name)
		}
	}
	differs := map[string]func(s *Store, id int64){
		"value": func(s *Store, id int64) { s.Put(ctx, id, "a", []byte(`1`)); s.Put(ctx, id, "b", []byte(`3`)) },
		"version": func(s *Store, id int64) {
			s.Put(ctx, id, "a", []byte(`1`))
			s.Put(ctx, id, "b", []byte(`0`))
			s.Put(ctx, id, "b", []byte(`2`))
		},
		"key name": func(s *Store, id int64) { s.Put(ctx, id, "a", []byte(`1`)); s.Put(ctx, id, "c", []byte(`2`)) },
		"extra key": func(s *Store, id int64) {
			s.Put(ctx, id, "a", []byte(`1`))
			s.Put(ctx, id, "b", []byte(`2`))
			s.Put(ctx, id, "c", []byte(`3`))
		},
		"missing key": func(s *Store, id int64) { s.Put(ctx, id, "a", []byte(`1`)) },
		// "ab"+"" vs "a"+"b" style ambiguity must not collide.
		"key boundary": func(s *Store, id int64) { s.Put(ctx, id, "a", []byte(`1`)); s.Put(ctx, id, "b2", []byte(``)) },
	}
	for name, f := range differs {
		if got := checksumFor(f); got == base {
			t.Errorf("%s did not change the checksum", name)
		}
	}
}

func TestDigestEntriesTenantFilterAndNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := tenant(t, s, "a"), tenant(t, s, "b")
	s.Put(ctx, a.ID, "k2", []byte(`"two"`))
	s.Put(ctx, a.ID, "k1", []byte(`"one"`))
	s.Put(ctx, b.ID, "other", []byte(`"x"`))

	d := digestOf(t, s, a.ID, true)
	if len(d.Tenants) != 1 || d.Tenants[0].TenantID != a.ID {
		t.Fatalf("filtered digest = %+v", d.Tenants)
	}
	sum := sha256.Sum256([]byte(`"one"`))
	want := []KeyDigest{
		{"k1", 1, hex.EncodeToString(sum[:])},
		{"k2", 1, hex.EncodeToString(func() []byte { x := sha256.Sum256([]byte(`"two"`)); return x[:] }())},
	}
	if fmt.Sprint(d.Tenants[0].Entries) != fmt.Sprint(want) {
		t.Fatalf("entries = %+v, want %+v", d.Tenants[0].Entries, want)
	}
	if _, err := s.Digest(ctx, 9999, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant err = %v, want ErrNotFound", err)
	}
	// Per-tenant checksum equals the one in the all-tenants digest.
	if all := digestOf(t, s, 0, false); byID(all, a.ID).Checksum != d.Tenants[0].Checksum {
		t.Fatal("per-tenant and all-tenant checksums differ")
	}
}

func TestDigestOnReplicaUsesAppliedSeqAndSeesOrphanTenants(t *testing.T) {
	ctx := context.Background()
	r := openReplicaDB(t)
	r.ApplyReplicated(ctx, tenantChange(1, 1, "t1", "h1"))
	r.ApplyReplicated(ctx, keyChange(2, 1, "k", OpPut, 4, `"v"`))
	r.ApplyReplicated(ctx, keyChange(3, 1, "gone", OpDelete, 1, "")) // no-op locally: no local change row
	// A key for a tenant whose record has not arrived (legacy primary): FK is off on replicas.
	r.ApplyReplicated(ctx, keyChange(4, 99, "orphan", OpPut, 2, `"o"`))

	d := digestOf(t, r, 0, true)
	if d.Seq != 4 { // applied_seq, not the replica's own (smaller) local seq
		t.Fatalf("seq = %d, want applied_seq 4", d.Seq)
	}
	if local, _ := r.LatestSeq(ctx); local >= 4 {
		t.Fatalf("test setup: local seq %d should be below applied_seq", local)
	}
	if len(d.Tenants) != 2 || d.Tenants[0].Name != "t1" || d.Tenants[1].TenantID != 99 || d.Tenants[1].Name != "" || d.Tenants[1].Keys != 1 {
		t.Fatalf("tenants = %+v", d.Tenants)
	}
	if e := d.Tenants[0].Entries; len(e) != 1 || e[0].Key != "k" || e[0].Version != 4 {
		t.Fatalf("entries = %+v (the primary's version must be kept)", e)
	}
}

// seq and data come from one snapshot: while a writer bumps a counter key, every
// digest must show the key at exactly the version its seq implies.
func TestDigestIsASnapshotConsistentWithSeq(t *testing.T) {
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

	deadline := time.Now().Add(1500 * time.Millisecond)
	checked := 0
	for time.Now().Before(deadline) {
		d, err := s.Digest(ctx, tn.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Tenants[0].Entries) == 0 {
			continue
		}
		if got, want := d.Tenants[0].Entries[0].Version, d.Seq-1; got != want {
			t.Fatalf("snapshot torn: seq %d but counter at version %d (want %d)", d.Seq, got, want)
		}
		checked++
	}
	close(stop)
	wg.Wait()
	if checked < 20 {
		t.Fatalf("only %d digests checked", checked)
	}
}
