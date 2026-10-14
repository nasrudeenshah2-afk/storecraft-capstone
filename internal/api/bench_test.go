package api

// Write latency with quota enforcement off and on (B14 release readiness: the
// quota check adds three small indexed reads to every write transaction).
// Run: go test -run XXX -bench BenchmarkPut -benchtime 3000x ./internal/api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

func benchmarkPut(b *testing.B, enforce bool) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	tok, _ := auth.NewToken()
	tn, err := st.CreateTenant(ctx, "bench", auth.HashToken(tok))
	if err != nil {
		b.Fatal(err)
	}
	if enforce {
		huge := int64(1 << 40) // never reached: this measures the check, not rejections
		if _, _, err := st.SetFlag(ctx, store.FlagQuotaEnforcement, true, 0); err != nil {
			b.Fatal(err)
		}
		if _, _, err := st.SetQuota(ctx, tn.ID, &huge, &huge, 0); err != nil {
			b.Fatal(err)
		}
	}
	srv := httptest.NewServer(New(st, Options{AdminToken: "bench-admin", PollTimeout: 300 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer srv.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := send(srv.URL, "PUT", fmt.Sprintf("/v1/keys/k%d", i%500), tok, `{"value":{"n":1,"s":"some text"}}`, nil)
		if r.err != nil || (r.status != 200 && r.status != 201) {
			b.Fatalf("put: %+v", r)
		}
	}
}

func BenchmarkPutQuotaOff(b *testing.B) { benchmarkPut(b, false) }
func BenchmarkPutQuotaOn(b *testing.B)  { benchmarkPut(b, true) }
