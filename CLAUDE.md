# storecraft

## Purpose
storecraft is a multi-tenant key-value service written in Go, backed by SQLite. It exposes an HTTP API, a change feed of writes, and a read-only replica mode. A Vue 3 + TypeScript admin console lives in `web/`.

## Repo structure
- `cmd/storecraft/` – main binary (wiring only)
- `internal/auth/` – tenant authentication and authorization
- `internal/store/` – SQLite-backed key-value storage
- `internal/api/` – HTTP handlers and routing
- `internal/feed/` – change feed
- `internal/replica/` – read-only replica mode
- `internal/metrics/` – metrics
- `api/` – `openapi.yaml`
- `scripts/` – PowerShell scripts: `ci.ps1` (full local CI), `snapshot.ps1` (release snapshot), `demo-up.ps1` / `demo-down.ps1` (local demo)
- `Dockerfile`, `docker-compose.yml`, `.dockerignore` – container images (Go server; console in `web/Dockerfile` + `web/nginx.conf`) and the primary + replica + console stack
- `releases/` – release snapshots (`releases/<name>/`: binary, console build, source zip, README, SHA256SUMS); snapshots replace Git tags
- `docs/DONE.md` – evidence log; `docs/capstone/` – capstone docs
- `web/` – Vue 3 + TypeScript admin console (rules in `web/CLAUDE.md`)

## Commands
- `go build ./...`
- `go vet ./...`
- `go test ./...` (use `-race` for concurrency code)
- `gofmt -l .` – must print nothing
- `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1` – the full local CI: gofmt, go vet, go test -race, then web lint, typecheck, unit tests, build, bundle budget and Playwright; stops at the first failure, exit 1 on failure
- `powershell -ExecutionPolicy Bypass -File scripts/snapshot.ps1 <name>` – runs CI, then builds `releases/<name>/`; never overwrites without `-Force`; `-SkipCI` only right after a green CI run
- `powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1` – fresh demo: seeds `demo.db` (tokens in `demo-tokens.txt`), starts primary :8090, replica :8091 and console :5173 in the background, records their PIDs in `demo-pids.json`; refuses if a port is busy or a demo is running. `scripts/demo-down.ps1` stops exactly those PIDs (checked by start time and path) and confirms the ports are free
- `docker compose up -d --build` – primary (:8090), replica (:8091) and console (:8080); needs `STORECRAFT_ADMIN_TOKEN` (synthetic); seed with `docker compose exec primary storecraft seed -db /data/storecraft.db`; `docker compose down -v` removes everything
- `storecraft verify -primary=<url> -replica=<url> [-timeout 30s]` – compares a replica with its primary via `/v1/admin/digest` (admin token in `STORECRAFT_ADMIN_TOKEN`, optional `STORECRAFT_REPLICA_ADMIN_TOKEN`); exit 0 equal, 1 diverged, 2 timeout or error; also compares per-tenant usage counters and quotas (STORE-210)
- `storecraft backfill-usage -db <file> [-limit N] [-restart]` – sets every tenant's usage to COUNT/SUM over its keys and rewrites old values in canonical form; one tenant per transaction, safe to run twice and while the server runs, a stopped run continues where it stopped
- `go test -race -count=20 -run Quota ./internal/...` – the STORE-210 concurrency gate (100 concurrent writers near the limit)

## Conventions
- Standard library first; add a dependency only if it is approved below.
- Return errors; do not panic for expected failures. Wrap with context (`%w`).
- Small, focused packages with minimal exported surface.
- HTTP routes are defined only in `routeSpecs` (`internal/api/routes.go`), and `api/openapi.yaml` must be updated with every route, method, status code or header change. `TestOpenAPI*` in `internal/api` enforces this.
- Metrics labels never contain tenant ids, keys or raw paths; use route patterns. The single exception (STORE-210, spec section 9): `storecraft_tenant_usage_keys` / `_bytes` carry `tenant_id` (bounded by the number of tenants); `TestMetricsLabelsUseRoutePatternNotRawPath` enforces it.
- Values are stored, replicated, counted and hashed in canonical JSON (`store.CanonicalJSON`); API responses are encoded without HTML escaping (`api.marshal`).
- Quota checks run inside the write transaction (`store.execOp` -> `checkQuota`), after the version preconditions; never check a quota outside the transaction that writes. Usage counters are maintained only by the `kv` triggers of migration 6 (and repaired by `backfill-usage`).
- The `quota_enforcement` flag is read from the database on every use (`store.Flag`, `checkQuota`); never cache it in memory.
- Tenant-facing feeds (`/v1/changes`, the SSE stream) must only ever read `kind = 'key'` rows of the caller's tenant; tenant records and other tenants' changes belong to the admin feed (`/v1/admin/changes`) only.
- A replica database (`replica_state` row) and a primary database are never interchangeable: a replica's own change seqs are local, `applied_seq` is the primary's seq. All replica writes go through `store.ApplyReplicated`. The one exception is `storecraft backfill-usage` on a replica database: it rewrites only the spelling of values (canonical JSON) and the usage counters, never a JSON value or a version, and it says so when it runs.
- Digests compare nodes only at equal `seq`; never compare digests taken at different seqs. Values are hashed in canonical-JSON form (see `store.Digest`).
- Multi-node tests use `internal/clustertest` (real primary/replica over httptest + temp SQLite).
- Tests live beside the code (`foo_test.go`).
- Tests use real HTTP (`httptest` servers or real listeners) and real concurrency (goroutines, `-race`); no mocking away the thing under test.

## Definition of done
1. `go build ./...`, `go vet ./...` pass and `gofmt -l .` is empty.
2. `go test ./...` passes (with `-race` where concurrency is involved).
3. New behavior has tests beside the code.
4. `scripts/ci.ps1` is green.
5. A row is added to `docs/DONE.md` (Task | Date | Evidence command | Result) with the real command run and its result.

## Approved dependencies
- Go: only `modernc.org/sqlite` (pure Go, no CGO).
- Web: listed in `web/CLAUDE.md`.
- Docker base images (A7, pinned): `golang:1.27.0-alpine`, `alpine:3.22.6`, `node:24.15.0-alpine`, `nginx:1.29.8-alpine`.

## Prohibited
- No Makefile. Git is used since 2026-10-14 (GitHub: nasrudeenshah2/storecraft-capstone, at the user's request); `.gitignore` keeps tokens, databases, binaries, node_modules and build output out of the repository. Never commit `demo-tokens.txt` or any token. Snapshots in `releases/` are still the release record.
- Never stop a process by name; only by a recorded PID (see `demo-down.ps1`).
- Never weaken, skip, or delete tests to make them pass; fix the code.
- No dependencies beyond the approved list; no CGO.
