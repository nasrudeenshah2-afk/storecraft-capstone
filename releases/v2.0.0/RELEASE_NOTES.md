# storecraft v2.0.0: per-tenant storage quotas (STORE-210)

Release date: 2026-10-14. Previous release: v1.0.0. Release candidate: v2.0.0-rc.1.

Every statement below was checked against the code and the tests; the "Evidence" column says where.

## New

| What | Details | Evidence |
|---|---|---|
| Per-tenant quotas | Each tenant has `max_keys` and `max_bytes` (null = unlimited). The default is 1000 keys and 1 MiB (1,048,576 bytes) until an admin edits the quota. | `internal/store/quota.go` `DefaultMaxKeys` / `DefaultMaxBytes`; `TestQuota_AC06_DefaultQuota` |
| Enforcement | With the `quota_enforcement` flag on, a write that would add a key over `max_keys`, or bytes over `max_bytes`, is rejected with **403 `quota_exceeded`**, including `details: {dimension, used, limit, requested}`. Nothing is written. Overwrites count only the size difference. Shrinking writes and deletes always work. | `TestQuota_AC01_KeyLimit`, `TestQuota_AC02_ByteLimitAndOverwriteDelta` |
| Correct under concurrency | The quota decision is made inside the same SQLite write transaction as the write. 100 concurrent writers near the limit never exceed it, in 20 of 20 race-detector runs. | `TestQuota_AC03_*`; `go test -race -count=20 -run Quota ./internal/...` |
| Usage API | `GET /v1/usage` (tenant token): own keys and bytes, used and limit, plus the change seq they correspond to. | `TestQuota_AC04_ReadUsageIsolatedPerTenant` |
| Admin API | `GET /v1/admin/tenants`, `GET /v1/admin/tenants/{id}`, `GET`/`PUT /v1/admin/tenants/{id}/quota` (If-Match required: 428 without it, 412 when stale). | `TestQuota_AC05_QuotaEditCAS`; `api/openapi.yaml` |
| Runtime flag | `GET /v1/flags` (tenant or admin token); `PUT /v1/admin/flags/quota_enforcement` (admin, If-Match). **Default off.** Changes apply from the next request, with no restart. | `TestQuota_AC11_*` |
| Console | Usage meter (keys and bytes, text plus an accessible meter, live updates); a clear over-quota message at save (input kept); an admin quota editor with a conflict dialog; an admin flag switch with confirmation. Everything quota-related is hidden while the flag is off. | `web/e2e/quota.spec.ts` (AC-UI-1..8, axe clean) |
| Replication | Replicas serve usage, quotas, tenants and the flag from replicated state; quota changes travel in the change log; a flag change reaches replicas at once. `storecraft verify` also compares usage and quotas. | `TestQuota_AC10_*` |
| Metrics | `storecraft_tenant_usage_keys{tenant_id}`, `storecraft_tenant_usage_bytes{tenant_id}`, `storecraft_quota_rejections_total{dimension}`, `storecraft_quota_enforcement_enabled`. Alert rule `QuotaRejectionsHigh` in `docs/capstone/alert.md`. | `TestQuotaMetrics`, `TestAlertRuleFiresInForcedScenario` |
| `storecraft backfill-usage` | Recomputes every tenant's usage and rewrites old values in canonical form. Idempotent, resumable (`-limit`, `-restart`), safe while the server runs. | `TestQuota_AC13_*`, `TestQuota_BackfillDuringLiveWrites` |

## Changed

- **Values are stored in canonical JSON:** compact, original key order, numbers exactly as sent, minimal string escapes. **API responses are no longer HTML-escaped** (`<` instead of `\u003c`). The JSON *value* a client reads is unchanged; only its spelling may differ. This fixes finding F1, a false `verify` divergence for values containing `<`, `>` or `&`. (`TestQuota_AC09_*`)
- `storecraft seed` prints tokens exactly as before (internally it now writes to a given output, so tests never log tokens).

## Fixed

- F1: `verify` reported a false divergence for values with `<`, `>` or `&`.
- A replica stopped during its very first step reported "replication stopped: applied seq: context canceled" instead of stopping cleanly (INC-1). (`TestRunStoppedBeforeItsFirstStepReturnsNil`)
- After the independent review (`docs/capstone/review-triage.md`; each fix has a regression test that failed before it):
  - **R1:** the admin quota editor could show (and save) the previous tenant's values after a quick tenant switch.
  - **R2:** `backfill-usage` now works in short batches. A large tenant used to hold the write lock for its whole run (36 s for 200,000 values), which could make server writes fail.
  - **R3:** `verify` interrupted with Ctrl+C now says "interrupted", not "timed out".
  - **R4:** the quota conflict dialog now shows an error when the newest values cannot be read.
  - **R5:** the admin flag panel now says when the setting cannot be read and offers "Try again".
  - **R6:** a replica whose flag version is ahead of the primary's now follows the primary.
  - **R7:** quota field errors appear on Save, not while typing.
  - **R9:** `backfill-usage` on a replica database says what it changes (spelling and counters only).

## Upgrade

1. Upgrade **replicas first**, then the primary. A v1.0.0 replica still follows a v2 primary (`TestReleaseV1ReplicaKeepsReplicatingFromNewPrimary`), but it serves no usage data.
2. Migration 6 runs automatically on start. It is additive; nothing is dropped or renamed.
3. Run `storecraft backfill-usage -db <file>` on every node's database.
4. Check with `storecraft verify`.
5. Review quotas, then turn enforcement on (console admin page).

Details: `docs/capstone/release-readiness.md`.

## Rollback

- To v2.0.0-rc.1 and back: drilled on the same data, zero divergence (`docs/capstone/drill-log.md`).
- To v1.0.0: works on the migrated data. v1.0.0 has no usage API, and its replica re-escapes `<`, `>` and `&` (F1 is back while it runs). After rolling forward again, run `backfill-usage` on every node.
- The first thing to do in an incident is to turn the flag off.

## Known limitations

- The console bundle is 115.4 of 120 kB (action item AI-6).
- The alert rule is tested with a small evaluator, not with Prometheus itself (AI-5).
- The blind game day and the human peer review are still to do (AI-1; release-readiness go/no-go row 7).

## Checksums

See `SHA256SUMS.txt` in this folder.
