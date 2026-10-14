package main

import (
	"context"
	"errors"
	"fmt"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

type seeded struct {
	Name  string
	Token string // empty when the tenant already existed
}

// seedTenants creates n synthetic tenants (demo-1..demo-n). Only token hashes
// are stored; the plaintext tokens are returned for one-time display.
func seedTenants(ctx context.Context, st *store.Store, n int) ([]seeded, error) {
	var out []seeded
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("demo-%d", i)
		token, err := auth.NewToken()
		if err != nil {
			return nil, err
		}
		_, err = st.CreateTenant(ctx, name, auth.HashToken(token))
		switch {
		case errors.Is(err, store.ErrTenantExists):
			out = append(out, seeded{Name: name})
		case err != nil:
			return nil, fmt.Errorf("seed %s: %w", name, err)
		default:
			out = append(out, seeded{Name: name, Token: token})
		}
	}
	return out, nil
}
