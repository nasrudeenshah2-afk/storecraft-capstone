# STORE-210 release readiness (v2.0.0)

Step B14, 2026-10-14. Labels: **FACT** (shown by a command or test named here), **INFERENCE** (reasoned, not measured), **UNKNOWN**.

## 1. SLO impact

The project has no written SLO, so this section uses the two signals an SLO would be built on: write latency and error rate.

| Signal | Impact | Evidence |
|---|---|---|
| Write latency | **No measurable change.** FACT: on this machine a key write takes 2.73–2.89 ms/op with enforcement off and 2.36–2.91 ms/op with it on (3 runs × 3000 writes each). The difference is inside the run-to-run noise. INFERENCE: the commit to disk dominates; the quota check adds three indexed reads in the same transaction. | `go test -run XXX -bench BenchmarkPut -benchtime 3000x -count 3 ./internal/api` (`internal/api/bench_test.go`) |
| Error rate | New, intended 403 `quota_exceeded`, **only after an admin turns enforcement on** (default off). No other new errors. Toggling under load gave zero unexpected failures. | `TestQuota_AC11_ToggleUnderConcurrentLoad` |
| Read paths | Unchanged, except that values are returned without HTML escaping. A JSON client reads the same value. | AC-API-9 tests |
| Replication lag | Unchanged mechanism. Quota rows add a few small change rows; a flag change wakes replicas at once. | AC-API-10 tests |
| Metrics cardinality | +2 series per tenant (the usage gauges), +3 fixed series. UNKNOWN: the real tenant count (spec section 12). | `TestQuotaMetrics` |

## 2. Rollout plan

**Dark launch:** the flag defaults off, so deploying changes no behaviour except canonical storage.

1. **Upgrade replicas first, then the primary.** FACT: a v1.0.0 replica also keeps replicating behind a new primary (`TestReleaseV1ReplicaKeepsReplicatingFromNewPrimary`, real v1.0.0 process). The order is still recommended, because until replicas are upgraded:
   - they serve no usage or quota data
   - `verify` cannot compare usage (spec section 8)
2. **Start the new primary.** Migration 6 runs automatically and is additive.
3. **Run `storecraft backfill-usage -db <primary db>` and the same on each replica database.** This canonicalises values stored by v1.0.0, which is needed so that `verify` compares equal, and repairs any counter. It is safe while serving and safe to run twice.
4. **Run `storecraft verify -primary=... -replica=...`.** Expect `OK: zero divergence`.
5. **Admins review the "Tenant quotas" page.** Defaults are 1000 keys / 1 MiB; raise them for tenants that are already above, before step 6.
6. **Turn `quota_enforcement` on** (admin page). Watch the signal in section 4 for 30 minutes.
7. **If it goes wrong, turn the flag off.** It takes effect on the next request; no restart, no data change.

## 3. Go / no-go criteria

| # | Criterion | Status |
|---|---|---|
| 1 | `scripts/ci.ps1` green on the release tree | FACT: green in B10 (`docs/capstone/ci-run-B10.log`, 9/9); re-run in B15 for v2.0.0 |
| 2 | `go test -race -count=20 -run Quota ./internal/...` green | FACT: exit 0 (DONE.md B7) |
| 3 | Rollback drill passes (rc.1 and back, same data) | FACT: PASS (`docs/capstone/drill-log.md`) |
| 4 | All 13 API and 8 UI ACs traced to passing tests | FACT: `docs/capstone/trace.md` |
| 5 | Independent review findings triaged; no open high-severity finding | see `docs/capstone/review-triage.md` (B13) |
| 6 | No tenant above its new quota without an admin decision | **No-go check at rollout** (step 5 above) |
| 7 | Human peer review approved | **UNKNOWN / open**: the playbook requires a human reviewer; Claude cannot provide one |

## 4. The ONE signal to watch after release

**`sum(rate(storecraft_quota_rejections_total[5m]))`**, with the `QuotaRejectionsHigh` alert (`alert.md`).

**Why this one:**
- Turning enforcement on is the only step that changes user-visible behaviour, and this counter counts exactly the users it affects. It counts 403 `quota_exceeded` and nothing else (`TestQuotaRejectionCounterCountsOnlyQuotaRejections`).
- A jump right after the toggle means a tenant's legitimate use was above its quota, or a client loops on the error.
- Both are fixed in seconds: raise the quota, or turn the flag off.
- Lag and error rate are already watched for every release and are unchanged by this one (section 1).

## 5. Rollback

FACT, from `docs/capstone/drill-log.md`:
- **To v2.0.0-rc.1 and back on the same data:** every verify reports zero divergence and every smoke check (sign in, read, write, usage) passes.
- **To v1.0.0 (the previous release):** it runs on the migrated data (also AC-API-12) and keeps serving keys. Expected caveats:
  - v1.0.0 has no usage API
  - its replica re-escapes `<`, `>`, `&` (finding F1), so `verify` reports such keys while v1.0.0 runs
  - after rolling forward, run `backfill-usage` on every node; the drill shows `verify` going from `usage_mismatch` to zero divergence after it

**Fastest mitigation, before any rollback:** turn the flag off.

## 6. Why the "contract" step is scheduled, not executed

Expand/contract: this release only **expands** the schema. New tables, a nullable column and triggers; nothing dropped, renamed or tightened (`TestQuotaMigration6IsAdditive`).

The **contract** step would tighten it, for example:
- a CHECK constraint on `changes.subject`
- a foreign key on `tenant_usage.tenant_id`
- making `changes.subject` mandatory for tenant rows

It is deliberately **not executed now**:
- **It would remove the rollback.** v1.0.0 writes rows that a tightened schema could reject. For example, its tenant rows have no `subject`, and SQLite cannot add constraints in place without rebuilding the table. The drill shows rollback to v1.0.0 works today; a contract step would make that path fail at the first write.
- **Old replicas would break.** A v1.0.0 replica could still be applying the log (section 2).
- **It brings no user value.** The application already enforces those rules.

It is scheduled as action item AI-7: for the release after v2.0.0, once v1.0.0 is no longer a supported rollback target and every node runs v2.x.
