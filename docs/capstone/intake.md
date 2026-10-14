# STORE-210 intake: per-tenant storage quotas, usage metering and console

Step B1, 2026-10-13. No code was changed in this step.

**Labels.** Every statement is labelled:
- **FACT**: read in the code or produced by a command that was actually run (file:line references are to the tree as of this date).
- **INFERENCE**: reasoned from facts, not tested.
- **UNKNOWN**: not decided or not checked yet.

**Method.**
1. FACT: a read-only subagent read every source file one by one: 53 Go files (cmd 7, internal 46), `api/openapi.yaml`, 33 files in `web/src`, 9 in `web/e2e`, 4 scripts and 3 Docker/nginx files. It reported, with line numbers, every place the ticket touches.
2. FACT: I then re-checked the key claims myself:
   - line spot-checks with grep
   - an absence search over 95 files for `quota`, `feature flag`, `enforcement`, `ListTenants`, `/v1/usage` and `/v1/admin/tenants`. The only hit was an unrelated comment, `store.go:81`.
   - a live reproduction for finding F1

---

## 1. The ticket (verbatim, from docs/prompts.md)

> Per-tenant storage quotas, usage metering and console. API: each tenant has a configurable maximum key count and maximum total value bytes; writes that would exceed quota are rejected in the standard error envelope (the spec decides the status code); a tenant can read its own usage; admins can set quotas. Usage must be correct under concurrency, survive restarts, replicate to replicas and be visible in metrics. Console: live usage meter per tenant, an admin quota editor that handles CAS conflicts, and a clear over-quota experience at the moment a save is rejected.

## 2. What exists today (baseline relevant to the ticket)

| # | Statement | Label | Where |
|---|---|---|---|
| B1 | There is no quota, usage, feature-flag or tenant-list code anywhere (searched 95 files). | FACT | grep over cmd/, internal/, api/, web/src, web/e2e |
| B2 | All primary writes (put, CAS put, create-only put, delete) go through one function, `Store.Apply` → `execOp`. They run inside `writeTx`, which starts the transaction with BEGIN IMMEDIATE on a single write connection. | FACT | `internal/store/apply.go:49-66, 77-122, 126-180`; `store.go:101, 110` |
| B3 | `execOp` never reads the old value's size, so an overwrite's byte difference cannot be computed today without an extra read in the same transaction. | FACT | `apply.go:133-175` |
| B4 | An idempotent replay returns before `execOp` (no write). A rejected write stores no idempotency record. | FACT | `apply.go:86-91, 104-115` |
| B5 | Replicas apply changes in `ApplyReplicated` (one transaction with `applied_seq`). An unknown change kind is an error, and the replica loop retries errors forever. | FACT | `store/replica.go:82-157, 148-149`; `replica/replica.go:107-133` |
| B6 | The change log's `kind` column allows only `'key'` and `'tenant'` (CHECK constraint from migration 4). SQLite cannot change a CHECK constraint without rebuilding the table. | FACT (constraint) / INFERENCE (rebuild needed) | `store/migrations.go:59` |
| B7 | There are 5 migrations. A new one would be number 6. The list is append-only. | FACT | `migrations.go:15-73` |
| B8 | New tenants are created only by `CreateTenant` (used by `storecraft seed`). There is no tenant HTTP API. | FACT | `store.go:173-201`; `cmd/storecraft/seed.go:19-38` |
| B9 | Routes: keys (GET/PUT/DELETE + list), changes, changes/stream, metrics, admin/changes, replication/status, admin/digest. No usage, quota, tenant-list or flag routes. | FACT | `internal/api/routes.go:26-35` |
| B10 | Tenant auth (`authenticate`) answers 401 for bad tokens and 403 `forbidden` for the admin token. Admin auth (`requireAdmin`) answers 401 for bad tokens and 403 for tenant tokens. `/metrics` answers 401 even for a tenant token. | FACT | `api.go:108-131`; `admin.go:26-48`; `api/metrics.go:75-79` |
| B11 | Error codes today: unauthorized, forbidden, not_found, invalid_request, precondition_failed, method_not_allowed, internal, idempotency_conflict, read_only_replica. The envelope has only `code` and `message`. | FACT | `api/errors.go:9-28`; `openapi.yaml:562-575` |
| B12 | PUT can already return 403 (`forbidden`, admin token on tenant data), so a 403 `quota_exceeded` shares the status. Clients must branch on `code`. | FACT | `api.go:118-120`; `openapi.yaml:86-87` |
| B13 | Metrics: requests, duration, changes written, idempotent replays and SSE streams. The code states "No label ever carries a tenant id or a key", and a test bans `tenant`/`tenant_id`/`key` labels. There is no labelled gauge type (GaugeVec). | FACT | `metrics/storecraft.go:3-4, 24-35`; `api/metrics_test.go:151-155`; `metrics/metrics.go:197` |
| B14 | Configuration is only command-line flags and environment variables fed into static `api.Options`. There is nothing runtime-toggleable. | FACT | `cmd/storecraft/main.go:58-77`; `api.go:30-39` |
| B15 | The console talks only to the primary (dev proxy and nginx). | FACT | `web/vite.config.ts:34-37`; `web/nginx.conf:31-36` |
| B16 | In the console, a failed save shows its message in the editor (`formError`), and unknown error codes become `unknown_error`. Extra envelope fields would be dropped. | FACT | `web/src/views/KeysView.vue:114-137`; `web/src/api/client.ts:61-77` |
| B17 | Delete events in the change feed carry `value_size: 0`, and overwrite events don't carry the old size, so a live meter cannot compute usage from the feed alone. | FACT | `store.go:151-155` |

## 3. Files and packages the ticket touches

Verdicts: **TOUCHED** = must change; **AFFECTED** = behaviour or tests depend on it and may need changes; everything else is not touched. All entries are FACT (read in the file); the "why" column is INFERENCE where it describes future work.

### Go
| Path | Verdict | Why |
|---|---|---|
| `internal/store/apply.go` | TOUCHED | Quota check and usage update must happen in `execOp`/`Apply`, inside `writeTx` (B2, B3). |
| `internal/store/store.go` | TOUCHED | `CreateTenant` (default quota), `Tenant` type, change kinds, new store functions (list tenants, read/set quota, read usage). |
| `internal/store/migrations.go` | TOUCHED | Migration 6: quota and usage tables, backfill. |
| `internal/store/replica.go` | TOUCHED | Replicas must keep usage and quotas in step inside `ApplyReplicated` (B5). |
| `internal/store/digest.go`, `snapshot.go` | AFFECTED | Digest already computes per-tenant keys and bytes (in compact JSON form); snapshot reads are reusable for a consistent usage read. |
| `internal/api/routes.go` | TOUCHED | New routes (usage, admin quota, admin tenants, flag). |
| `internal/api/api.go`, `admin.go` | TOUCHED | Auth for "tenant reads own, admin reads any" fits neither existing helper (B10). The flag must reach requests. |
| `internal/api/writes.go` | TOUCHED | Map the store's quota error to 403 `quota_exceeded`; count rejections. |
| `internal/api/errors.go` | TOUCHED | New code, plus `used`/`limit` in the envelope (B11). |
| `internal/api/metrics.go`, `changes.go`, `stream.go` | AFFECTED | Metrics route; change JSON (`value_size`); SSE only sends `event: change`. |
| `internal/metrics/storecraft.go`, `metrics.go` | TOUCHED | New metrics. Needs a labelled gauge or a scrape-time collector (B13). |
| `internal/feed/feed.go` | AFFECTED | Wake-ups on new commit types are harmless re-queries. |
| `internal/replica/replica.go` | TOUCHED | Wire format and handling of any new change kind. |
| `internal/verify/verify.go` | AFFECTED | B7 asks verify/digest to also compare usage. |
| `cmd/storecraft/main.go`, `seed.go` | AFFECTED | New `backfill-usage` command (B6); seeded tenants get default quotas. |
| `internal/clustertest/clustertest.go` | AFFECTED | Test cluster options for the flag; it says "there is no tenant API". |
| `api/openapi.yaml` | TOUCHED | Contract first (B3): new paths, error code, envelope fields, schemas. |

### Go tests that hard-code values a quota feature can change (AFFECTED)
These tests assert exact counts that change if tenant creation or quota edits add change-log rows, or if a write logs more than one row:
- `internal/store`: `store_test.go:63` (5 migrations), `apply_test.go:48`, `changes_test.go:156-169`, `digest_test.go:89-92, 231`, `snapshot_test.go:22, 72`, `replica_test.go:49, 66, 74, 88, 227, 242`
- `internal/api`: `admin_test.go:57-62, 255, 262`, `digest_test.go:51`, `idempotency_test.go` (change counts), `metrics_test.go:151-155` (banned labels), `openapi_test.go:113-125, 168-176` (status and error-code tables), `api_test.go:403-407` (a value about 100 bytes under 1 MiB)
- `internal/replica/replica_test.go:349, 460-471, 733`; `internal/verify/verify_test.go:191-212`; `cmd/storecraft/node_test.go:118`

### Web
| Path | Verdict | Why |
|---|---|---|
| `web/src/api/client.ts` | TOUCHED | Usage, quota, tenants and flag calls; keep `used`/`limit` from the envelope (B16). |
| `web/src/api/errors.ts`, `schema.d.ts` (regenerated) | TOUCHED | New error code and message; `ApiError` details. |
| `web/src/views/KeysView.vue`, `useKeys.ts` | TOUCHED | Over-quota experience at save time; usage meter placement. |
| `web/src/views/AdminHomeView.vue`, `App.vue` | TOUCHED | Admin quota editor; flag-dependent visibility. |
| `web/src/components/ConflictDialog.vue` | AFFECTED | Wording and props are key-specific; the quota editor's 412 handling needs a generalised dialog. |
| `web/src/components/ReplicationBadge.vue` | AFFECTED | A polling pattern to copy for usage. |
| `web/src/composables/useChangeFeed.ts` | AFFECTED | Only `event: change` is handled; usage can't be derived from it (B17). |
| `web/src/stores/session.ts` | AFFECTED | Roles; a flag store may sit beside it. |
| `web/src/testing/fakeServer.ts` | TOUCHED | Needs quota, usage and flag behaviour. Its `value_size` uses JS string length, not UTF-8 bytes. |
| `web/src/api/*.test.ts`, `views/*.test.ts` | AFFECTED | Error-code tables and 403 mapping. |
| `web/e2e/fixtures.ts` | TOUCHED | Admin quota and flag helpers. |
| `web/e2e/cluster.ts`, `global-setup.ts`, `*.spec.ts`, `playwright.config.ts` | AFFECTED | One shared server and shared tenants: quota changes in one spec must not leak into others. |

### Not touched
FACT: `internal/auth/*`, `internal/metrics/metricstest/*`, `web/src/api/sse.ts`, `useApi.ts`, the base components (BaseButton, BaseModal, BaseTextField, ToastHost: reused), `scripts/*.ps1`, `Dockerfile`, `docker-compose.yml`, `web/nginx.conf` (new `/v1` routes are proxied automatically).

## 4. Findings during intake

**F1 – Primary and replica store different bytes for the same value (existing bug, reproduced).**
- FACT, reproduced on 2026-10-13: I ran a throw-away demo (primary :18090, replica :18091), wrote `{"value":"<b>Tom & Jerry</b>"}` on the primary, and ran `storecraft verify`. It printed `DIVERGED: 1 difference(s)`, exit code **1**, for a healthy replica: the two sides had different sha256 values for key `html`.
- FACT, the cause in the code: the primary stores the client's raw JSON (`internal/api/writes.go:76`, `parseValue` :184-203). The replica receives the value through the admin feed, which re-encodes it with Go's JSON encoder (`internal/api/admin.go:95`). That encoder compacts whitespace *and* rewrites `<`, `>` and `&` as `<`, `>` and `&`. The digest only removes whitespace differences (`internal/store/digest.go:46-52`).
- FACT: both GETs still look identical to clients, because responses go through the same encoder on the way out.
- INFERENCE: this matters for STORE-210 as well as `verify`. "Total bytes of values" would differ between primary and replica for the same data.

**F2 – Where the quota check must live.**
- FACT: only inside `writeTx` (BEGIN IMMEDIATE, single write connection) are the check and the write atomic.
- INFERENCE: checking in the API layer, or through the read pool, would be a check-then-write race.

**F3 – Change-log kind constraint.** FACT: `kind IN ('key','tenant')`. INFERENCE: a new `quota` change kind needs either a table rebuild, which conflicts with B6's "additive only" rule, or another way to carry quota changes to replicas.

**F4 – Old replicas and new change kinds.**
- FACT: an unknown kind makes `ApplyReplicated` fail, and the replica retries forever.
- INFERENCE: during a rolling upgrade a v1.0.0 replica would stall silently behind a newer primary that writes a new kind.

**F5 – Metrics rule conflict.** FACT: B7 asks for per-tenant usage gauges (`storecraft_tenant_usage_keys`/`_bytes`, "labels bounded by the number of tenants"), but the code and a test forbid tenant labels (B13).

**F6 – 403 is shared.** FACT: `forbidden` and `quota_exceeded` would both be 403 on PUT (B12). INFERENCE: the console and tests must use `code`, which is already the project rule.

**F7 – Default quota is close to an existing test.** FACT: `TestBodyJustUnderLimitAccepted` writes one value about 100 bytes under 1 MiB (`api_test.go:403-407`). INFERENCE: it still fits a 1 MiB default for an empty tenant, with almost no margin.

## 5. Questions asked and answers

Answers 1–9 come from the product-owner table in `docs/prompts.md` (FACT: that is what the table says). Questions 10–17 are not answered there. They are **asked now** (in the B1 report) and recorded as UNKNOWN until answered; each has my recommendation for the B2 spec.

| # | Question | Answer | Source / label |
|---|---|---|---|
| 1 | What counts as usage? | Key count and total bytes of values (UTF-8 length). Key names do not count. | PO table – FACT |
| 2 | Default quota? | 1000 keys and 1 MiB per tenant; `null` = unlimited. | PO table – FACT |
| 3 | Over-quota response? | HTTP 403, code `quota_exceeded`, message includes used and limit. Not 429 (retrying won't help). | PO table – FACT |
| 4 | Overwrite? | Counts only the byte difference; a shrinking write is always allowed. | PO table – FACT |
| 5 | Delete? | Frees quota. | PO table – FACT |
| 6 | Admin lowers a quota below current usage? | Allowed; the tenant is then over quota: growing writes rejected, deletes and shrinking overwrites allowed. | PO table – FACT |
| 7 | Who can edit quotas? | Admin token only; quota records have a `version` and use If-Match (CAS). | PO table – FACT |
| 8 | Who can read usage? | A tenant reads its own via `GET /v1/usage`; admin reads any tenant. | PO table – FACT |
| 9 | Feature flag? | `quota_enforcement`, runtime-toggleable by admin, no restart. Off = no rejections and the UI hides the feature. | PO table – FACT |
| 10 | Which bytes count: what the client sent, or a canonical form? Today primary and replica store different bytes (F1). | UNKNOWN. **Recommend:** canonical compact JSON *without* HTML escaping, stored that way on both primary and replica (fixes F1 too), so "UTF-8 length" is the same everywhere. | asked in B1 |
| 11 | Order of checks when several apply (replica 409, auth, body 400, idempotent replay, 412/404 precondition, 403 quota)? | UNKNOWN. **Recommend:** replica 409 → auth 401/403 → body 400 → idempotent replay (never counted, never rejected) → precondition 412/404 → quota 403. | asked in B1 |
| 12 | Per-tenant usage gauges need a tenant label, which the current rule forbids (F5). | UNKNOWN. **Recommend:** allow a `tenant_id` label on the two usage gauges only (bounded by the number of tenants, as B7 says); keep it banned everywhere else; update the rule and the test together. | asked in B1 |
| 13 | Flag default when the feature ships: on or off? | UNKNOWN. **Recommend:** off on upgraded databases (dark launch; turn on deliberately), and a documented admin toggle. | asked in B1 |
| 14 | Is "1 MiB" 1,048,576 bytes? | INFERENCE: yes (matches the API body limit `maxBodyBytes = 1<<20`, `api.go:20`). Confirm in B2. | asked in B1 |
| 15 | Do replicas serve `GET /v1/usage` (read-only, from replicated data)? | UNKNOWN. **Recommend:** yes, read-only, with the same answer as the primary at the same seq; quota edits only on the primary (409 on replicas). | asked in B1 |
| 16 | How do quota changes reach replicas, given F3/F4? | UNKNOWN. To decide in B2/B7 (for example: a new change kind via an additive way to record it, with replica upgrade order documented). | asked in B1 |
| 17 | Should a rejected write with an Idempotency-Key, retried after the admin raises the quota, execute? | INFERENCE: yes, with today's code (rejections store no idempotency record, B4). **Recommend:** keep it so and document it. | asked in B1 |

## 6. Assumptions (to confirm in the B2 spec)

- INFERENCE: usage will be kept as counters (a `tenant_usage` row per tenant), updated in the same transaction as each write, not recomputed by scanning `kv`. Scanning on every write would read up to 1 MiB × 1000 keys.
- INFERENCE: quotas will live in their own table (`tenant_quotas`, with a `version` for CAS), as the B6 prompt describes.
- INFERENCE: existing tenants get the default quota and a backfilled usage when the migration and `backfill-usage` run (B6).
- INFERENCE: the console keeps talking only to the primary (B15). The usage meter reads `GET /v1/usage` and refreshes on change-feed events.
- INFERENCE: `GET /v1/admin/tenants` is a new lightweight list (id, name). The digest route is too expensive for this purpose (it reads every value).

## 7. UNKNOWNs (not checked or not decided)

- UNKNOWN: questions 10–17 above.
- UNKNOWN: whether the v1.0.0 binary keeps working against the migrated database (B6 requires checking this).
- UNKNOWN: how many tenants a real deployment has (it bounds the per-tenant metrics cardinality).
- UNKNOWN: whether quota and usage should appear in the tenant-facing change feed, or only in the admin feed.

## 8. Next step

B2 (spec): propose the schema, endpoints and error shapes, using the answers above, and resolve questions 10–17 with the product owner/reviewer.
