# STORE-210 delivery plan and risk register

Step B4, 2026-10-13. Inputs: `spec.md` (approved), `intake.md`.

**Process notes (honest record).**
- The prompt asks to "switch to Plan Mode". The user asked me to complete all remaining steps without stopping for approvals and to review everything at the end, so this plan was written directly instead of through Plan Mode, which would pause for approval.
- The prompt also asks the user to challenge the plan at least once. The user delegated that, so the challenge recorded below was raised by me, standing in for the reviewer, and is marked as such.
- Both versions are kept, as required.

---

## Revision 1 (first version)

### Stream A: backend (each step ships behind the flag, which defaults to off)
| # | Step | Ships when | Depends on |
|---|---|---|---|
| A1 | **Canonical values (fixes F1).** One canonicaliser (compact JSON, no HTML escaping, original key order); puts store canonical bytes; the admin feed sends them unchanged; replicas canonicalise on apply; the digest uses the same canonicaliser. | `verify` reports zero divergence for `<`, `>`, `&` and whitespace values | — |
| A2 | **Migration 6** (B6): `tenant_quotas`, `tenant_usage`, `flags`, `backfill_progress`, `changes.subject`, plus usage triggers on `kv` and an initial usage computation. | v1.0.0 binary still runs on the migrated DB; usage equals COUNT/SUM | A1 |
| A3 | **`storecraft backfill-usage`** (B6): per tenant, one transaction; canonicalises old values and recomputes usage; idempotent and resumable. | run twice changes nothing; resumes after interruption | A2 |
| A4 | **Read APIs**: `GET /v1/usage`, `/v1/admin/tenants[/{id}]`, `GET .../quota`, `GET /v1/flags`. | contract tests green for the GETs | A2 |
| A5 | **Write APIs**: quota PUT and flag PUT with CAS (428/412), 409 on replicas. | contract tests green | A4 |
| A6 | **Enforcement** in the key-write path, from the store's usage and quota. | ACs 1, 2, 5, 7 green | A5 |
| A7 | **Replication of quotas and flags**; digest and verify compare usage and quota. | primary+replica test equal after load | A5 |
| A8 | **Metrics**: usage gauges, rejection counter, enabled gauge. | metrics tests green | A6 |

### Stream B: frontend (after the backend read and write APIs exist)
| # | Step | Ships when |
|---|---|---|
| B1 | Flag store (polls `/v1/flags`) and the client-event module | flag hides the quota UI |
| B2 | Usage meter (states, live refresh) | AC-UI-1, AC-UI-2 |
| B3 | Over-quota experience at save | AC-UI-3 |
| B4 | Admin quota editor + generalised conflict dialog | AC-UI-4, AC-UI-5 |
| B5 | Admin flag toggle with confirmation | AC-UI-7 |

### Rev 1 enforcement sketch
The key-write handler reads the tenant's usage and quota through the store (read pool), decides, and then calls `Store.Apply` to write. Usage triggers update `tenant_usage` when the write commits.

---

## Challenge (raised by me, standing in for the reviewer)

> "What happens if two requests arrive at the same moment?" Concretely: a tenant has 1 free key and two PUTs for two new keys arrive together. Also: an admin lowers the quota while writes are in flight; two admins save the quota at once; `backfill-usage` runs while the server writes; the same idempotent request is sent twice at once.

## Revision 2 (current; what changed and why)

1. **The quota check moved into the write transaction (the Rev 1 sketch was a race).**
   - In Rev 1 both PUTs could read "999 of 1000 used" from the read pool before either wrote, and both would be accepted: 1001 keys. That is the check-then-write race.
   - Now the check runs **inside `writeTx`**, which starts with `BEGIN IMMEDIATE` on the single write connection (FACT: `store.go:101,110`, `apply.go:49-66`). It reads the existing row's version and length, applies the preconditions (412/404), reads usage, quota and the flag **in that same transaction**, decides, and only then writes.
   - The second request waits for the write lock and sees the first one's committed usage. This is proven by the 100-writer test in AC-API-3, run with `-race -count=20`.
2. **Quota edits and flag toggles go through the same write lock.** A quota lowered "at the same moment" as a write is either fully before it (the write is checked against the new quota) or fully after it (the write was checked against the old one). There is no in-between.
3. **Two admins saving at once:** CAS on `version`. Exactly one gets 200; the other gets 412 and sees the conflict dialog. A store-level race test has exactly one winner.
4. **Backfill vs live writes:**
   - `backfill-usage` processes one tenant per `BEGIN IMMEDIATE` transaction. Live writes for that tenant wait a few milliseconds and never interleave.
   - The usage triggers keep counters right for writes before and after, so the backfill is a repair and canonicalisation tool, not a race window.
   - Resumability is through `backfill_progress`.
5. **Same idempotent request twice at once:** both reach `Apply` serialised by the write lock. The first executes and stores its record; the second finds the record and replays (FACT: `apply.go:86-91`). A replay is never quota-checked or counted. A test sends 20 identical requests at once near the limit.
6. **Added a concurrency test for each of the above** to B5 and B7 (they were not in Rev 1).
7. **Cross-process note:** the CLI and the server are separate processes on one file. `BEGIN IMMEDIATE` with `busy_timeout` serialises them as well. INFERENCE (to be confirmed by the backfill-during-load test): no extra locking is needed.

Stream order and steps otherwise unchanged from Rev 1.

---

## Risk register

L = likelihood, I = impact (H/M/L).

| # | Risk | Area | L | I | Mitigation | Detection signal |
|---|---|---|---|---|---|---|
| R1 | **Check-then-write race** lets concurrent writes exceed a quota | API/data | H without the fix | H | Check inside `writeTx` (Rev 2 item 1) | AC-API-3 100-writer test under `-race -count=20`; `verify` usage vs scan |
| R2 | **Usage drift on overwrite/delete** (counters disagree with data) | data | M | H | SQLite triggers on `kv` maintain counters in the same transaction for every writer; overwrite uses new−old length | every API acceptance test compares `GET /v1/usage` with a direct COUNT/SUM over `kv`; `verify` reports `usage_mismatch` when primary and replica counters differ (updated in B7: verify compares nodes, not counters vs. a scan) |
| R3 | **Backfill double-run or crash** double-counts or leaves partial state | data | M | M | Backfill *sets* usage to COUNT/SUM (never adds), per-tenant transactions, `backfill_progress` | AC-API-13 tests: twice = no change; interrupted then resumed = same result |
| R4 | **v1.0.0 binary breaks, or corrupts usage, on the new schema** (rollback) | data/release | M | H | Migration only adds; triggers keep usage right for v1 writes; missing quota row = default | AC-API-12 test runs `releases/v1.0.0/storecraft.exe` on a migrated DB; rollback drill (B14) |
| R5 | **Replica lag shows stale usage** | data | M | L | Responses include `seq`; the console only talks to the primary; `verify` compares at equal seq | replication status `lag`; `verify` timeout instead of a false divergence |
| R6 | **Stale usage meter in the UI** after another client writes or an admin edits | UI | M | M | Refresh on every change event (debounced) and every 10 s; refresh on stream reconnect | AC-UI-2 Playwright test (≤2 s); manual two-window test |
| R7 | **Flag state differs between primary and replica** (or between UI and API) | data/UI | M | M | Flag stored on the primary, carried in the admin feed with a version; replica requests wake on flag change; UI polls `/v1/flags` every 3 s | AC-API-10 test: replica `GET /v1/flags` equals the primary's and follows a toggle; AC-API-11 toggle-under-load test; AC-UI-6 (updated in B7: verify does not compare flags, because they are not in the seq-ordered log) |
| R8 | **Canonicalisation changes values clients care about** (number text, key order, escapes) | data | L | H | The canonicaliser keeps number text verbatim (`json.Number`) and original key order; only whitespace and string escapes change | canonical round-trip unit tests; digest equality tests |
| R9 | **An old v1.0.0 replica stalls** on new change rows | data/release | L | M | Quota rows use kind `tenant` (a v1 replica sees an existing tenant and does nothing); flags travel outside the log | test with a v1-shaped replica apply; upgrade order "replicas first" in the release plan |
| R10 | **Per-tenant metrics cardinality** grows with tenants | ops | L | M | `tenant_id` label only on the two usage gauges; values read at scrape time | `/metrics` size; documented bound in the spec |
| R11 | **Over-quota UX loses user input or is unclear** | UI | M | M | Editor stays open; message from `details`; tests check that the input is kept | AC-UI-3 tests; axe |

## Test plan summary (where each AC is proven)
- **API ACs 1–13:** Go tests over real HTTP and real temp SQLite in `internal/api` (and `internal/store` for unit-level), plus `cmd/storecraft` for the CLI and the v1.0.0 binary.
- **UI ACs 1–8:** Playwright against the real server (written first, in B5), plus Vitest unit tests for the new composables and components (in B8).
- The trace table (B9) maps every AC to its tests and code.

## Findings during delivery (kept here so the plan stays honest)

- **B6: Rev 2 item 7 was partly wrong (INFERENCE disproved by a test).** "BEGIN IMMEDIATE with busy_timeout serialises the CLI and the server; no extra locking is needed" was not enough. `TestQuota_BackfillDuringLiveWrites` showed that under a steady stream of server writes, the separate backfill process waited longer than busy_timeout (5 s) and failed with `SQLITE_BUSY`, because SQLite's busy handler is not fair. Fix: the backfill retries a transaction that got `SQLITE_BUSY` (it has changed nothing) with jittered backoff for up to 2 minutes (`writeTxRetryBusy`, typed error code check). The correctness argument (one tenant per transaction; triggers for live writes) was right and is unchanged.
- **B6: trigger conflict policy.** The first version of the usage triggers used `INSERT OR IGNORE`; an UPSERT on `kv` (`ON CONFLICT DO UPDATE`, used by this build and by v1.0.0) overrides the conflict policy of statements inside its triggers, which raised `UNIQUE constraint failed: tenant_usage.tenant_id`. Found by `TestQuotaUsageTriggersFollowEveryWrite`; the triggers now use `INSERT ... WHERE NOT EXISTS`.

## B10: the flag and observability, as built

**Where the flag lives.** `quota_enforcement` is one row in the `flags` table of the **primary's** database (migration 6). Values: enabled 0/1, plus a `version` for CAS. No row means off at version 0.

**How every request reads it, with no restart.** Nothing holds it in memory.
- **Key writes:** the quota check reads the flag inside the write transaction (`store.checkQuota` → `readFlag`), so the very next write after a toggle obeys it. Mutation M3 (flag cached once) made both AC-API-11 tests fail.
- **`GET /v1/flags`, the enforcement gauge and the admin feed:** these read it through `store.Flag` on every call.

**How replicas and the UI agree.**
- **Replicas:** the admin feed carries the flag in its `flags` field. A replica stores it with `ApplyReplicatedFlag` (newer version wins) and sends `flags_version` with every pull, so the primary answers at once when the replica's copy is stale.
- **The console:** it reads the flag only through the Pinia store `web/src/stores/flags.ts`, polled every 3 s while signed in, so every open console follows a toggle within 5 s (AC-UI-6).

**Toggling.** `PUT /v1/admin/flags/quota_enforcement` uses If-Match (412 when stale, 428 when missing) and is logged with old and new state. The CAS and idempotency behaviour of key writes is unchanged; the toggle-under-load test sends 20 writers through 6 toggles with zero unexpected failures.

**Observability.**
- **Metrics (spec section 9):** usage gauges per tenant (read at scrape time), `storecraft_quota_rejections_total{dimension}`, and `storecraft_quota_enforcement_enabled`.
- **Client events:** five, recorded in the browser only (`web/src/events.ts`).
- **Alert rule:** `QuotaRejectionsHigh` in `docs/capstone/alert.md`, with a test that forces it to fire.
