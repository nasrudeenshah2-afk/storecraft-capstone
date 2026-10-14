package main

import (
	"context"
	"path/filepath"
	"testing"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

func TestSeedTenants(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	res, err := seedTenants(ctx, st, 3)
	if err != nil || len(res) != 3 {
		t.Fatalf("seed = %+v, %v", res, err)
	}
	for _, r := range res {
		if r.Token == "" {
			t.Fatalf("%s has no token", r.Name)
		}
		got, err := st.TenantByTokenHash(ctx, auth.HashToken(r.Token))
		if err != nil || got.Name != r.Name {
			t.Fatalf("token for %s does not resolve: %+v, %v", r.Name, got, err)
		}
	}
	// Second run must not fail or reveal new tokens.
	again, err := seedTenants(ctx, st, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Token != "" {
			t.Fatalf("%s re-seeded with a token", r.Name)
		}
	}
}
