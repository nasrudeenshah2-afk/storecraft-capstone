package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

type digestResp struct {
	Role    string `json:"role"`
	Seq     int64  `json:"seq"`
	Tenants []struct {
		TenantID   int64  `json:"tenant_id"`
		Name       string `json:"name"`
		Keys       int64  `json:"keys"`
		ValueBytes int64  `json:"value_bytes"`
		Checksum   string `json:"checksum"`
		Entries    []struct {
			Key     string `json:"key"`
			Version int64  `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"entries"`
	} `json:"tenants"`
}

func (e *env) digest(query string) (digestResp, resp) {
	e.t.Helper()
	r := e.do("GET", "/v1/admin/digest"+query, adminToken, "", nil)
	var d digestResp
	if r.status == 200 {
		if err := json.Unmarshal([]byte(r.body), &d); err != nil {
			e.t.Fatalf("decode %q: %v", r.body, err)
		}
	}
	return d, r
}

func TestDigestEndpointShapeAndValues(t *testing.T) {
	e := setup(t)
	// The client's own whitespace is stored as sent; the digest hashes compact JSON.
	e.put(e.tokenA, "k1", `{"value":{"a": 1}}`)
	e.put(e.tokenA, "k1", `{"value":{"a": 2}}`)
	e.put(e.tokenA, "k2", `{"value":"text"}`)
	e.put(e.tokenB, "other", `{"value":[1, 2]}`)

	d, r := e.digest("")
	e.expect(r, 200, "")
	if d.Role != "primary" || d.Seq != 6 || len(d.Tenants) != 2 { // 2 tenant rows + 4 writes
		t.Fatalf("digest = %+v", d)
	}
	a, b := d.Tenants[0], d.Tenants[1]
	if a.Name != "a" || a.Keys != 2 || a.ValueBytes != int64(len(`{"a":2}`)+len(`"text"`)) || len(a.Checksum) != 64 || a.Entries != nil {
		t.Fatalf("tenant a = %+v", a)
	}
	if b.Name != "b" || b.Keys != 1 || b.ValueBytes != int64(len(`[1,2]`)) || b.Checksum == a.Checksum {
		t.Fatalf("tenant b = %+v", b)
	}
	if strings.Contains(r.body, `"entries"`) {
		t.Fatalf("summary digest carries per-key entries: %s", r.body)
	}

	// One tenant with its per-key digests.
	one, r := e.digest("?tenant=1")
	e.expect(r, 200, "")
	if len(one.Tenants) != 1 || one.Tenants[0].Checksum != a.Checksum || one.Seq != d.Seq {
		t.Fatalf("tenant digest = %+v (all-tenants checksum %s)", one, a.Checksum)
	}
	sum := sha256.Sum256([]byte(`{"a":2}`))
	if got := one.Tenants[0].Entries; len(got) != 2 || got[0].Key != "k1" || got[0].Version != 2 || got[0].SHA256 != hex.EncodeToString(sum[:]) || got[1].Key != "k2" {
		t.Fatalf("entries = %+v", got)
	}
	// A tenant with no keys still returns an (empty) entries array.
	if _, err := e.st.CreateTenant(context.Background(), "nokeys", "hash-nokeys"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/admin/digest?tenant=3", adminToken, "", nil); !strings.Contains(r.body, `"entries":[]`) || !strings.Contains(r.body, `"keys":0`) {
		t.Fatalf("empty tenant digest = %s", r.body)
	}

	// Errors are standard envelopes.
	for _, q := range []string{"?tenant=abc", "?tenant=0", "?tenant=-1", "?tenant="} {
		e.expect(e.do("GET", "/v1/admin/digest"+q, adminToken, "", nil), 400, "invalid_request")
	}
	e.expect(e.do("GET", "/v1/admin/digest?tenant=9999", adminToken, "", nil), 404, "not_found")
}

// The digest follows the data: any write changes the checksum and the seq.
func TestDigestMovesWithWrites(t *testing.T) {
	e := setup(t)
	e.put(e.tokenA, "k", `{"value":1}`)
	before, _ := e.digest("")
	e.put(e.tokenA, "k", `{"value":1}`) // same value, new version
	after, _ := e.digest("")
	if after.Seq != before.Seq+1 || after.Tenants[0].Checksum == before.Tenants[0].Checksum {
		t.Fatalf("a rewrite with the same value must change seq and checksum: %+v -> %+v", before, after)
	}
	if after.Tenants[1].Checksum != before.Tenants[1].Checksum {
		t.Fatal("another tenant's checksum changed")
	}
	again, _ := e.digest("")
	if again.Tenants[0].Checksum != after.Tenants[0].Checksum || again.Seq != after.Seq {
		t.Fatal("digest of unchanged data is not stable")
	}
}
