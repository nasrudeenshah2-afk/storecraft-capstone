# STORE-210 trace table

Step B9, 2026-10-13. Requirement (spec.md section) → acceptance criterion → tests → code → evidence command. Every AC of `spec.md` is listed; the CI run that proves them all at once is in `docs/capstone/ci-run-B9.log`.

Paths are relative to the repository root; `:N` is the line where the function or test starts.

## Integration (B9 step 1)

- **No conflicts.** FACT: there is one working tree (no Git, no branches, by project rule), and the backend (B6–B7) and frontend (B8) were built in sequence against the same `api/openapi.yaml`. Nothing had to be merged, so there was nothing to choose between.
- **Contract alignment, checked rather than assumed.**
  - The console types come from the spec (`npm run gen:api` → `web/src/api/schema.d.ts`), and `npm run typecheck` passes.
  - The Go route-coverage test (`TestOpenAPIDocumentsExactlyTheRouterRoutes`) and the status-code test pass.
  - The Playwright suite runs the console against the real Go server built from this tree (31/31).
- One additive contract change during B7: the optional `flags_version` query parameter of `/v1/admin/changes`, which replicas use. It is documented in `api/openapi.yaml`. The console does not use that route.

## Shared evidence commands

| Name | Command |
|---|---|
| GO-AC | `go test -race -count=1 -run 'TestQuota\|TestContractSTORE210' ./...` |
| GO-20 | `go test -race -count=20 -run Quota ./internal/...` |
| VT | `cd web; npm run test` |
| PW | `cd web; npx playwright test e2e/quota.spec.ts` |
| CI | `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1` (all of the above except GO-20, plus lint, typecheck, build, bundle) |

## API acceptance criteria (spec section 6)

| Requirement | AC | Tests | Code | Evidence |
|---|---|---|---|---|
| §5.1, §5.2 limits are enforced; nothing is written on rejection | **AC-API-1** key limit | `internal/api/quota_acceptance_test.go:272` `TestQuota_AC01_KeyLimit`; `internal/api/contract_store210_test.go` `TestContractSTORE210_QuotaExceededEnvelope` | `internal/store/apply.go:134` `execOp` (precondition, then quota, in the transaction); `internal/store/quota.go:163` `checkQuota`; `internal/api/writes.go:93` `apply` (403 + details) | GO-AC |
| §5.2 overwrite counts the delta; shrinking and deletes are always allowed | **AC-API-2** byte limit | `quota_acceptance_test.go:303` `TestQuota_AC02_ByteLimitAndOverwriteDelta` | `apply.go:134` (`addBytes = new − old`), `quota.go:163` (keys checked before bytes); triggers in `internal/store/migrations.go:87` (migration 6) | GO-AC |
| §1 correct under concurrency (plan R1) | **AC-API-3** | `quota_acceptance_test.go:347` / `:369` / `:391` (100 concurrent writers: keys, bytes, mixed) | the quota check runs inside `writeTx` (`BEGIN IMMEDIATE`, single write connection), `apply.go:134`; usage triggers in migration 6 | GO-20 (exit 0, 2026-10-13); mutation M1 in DONE.md B7 |
| §4 usage reads, tenant isolation, admin-only routes | **AC-API-4** | `quota_acceptance_test.go:424` `TestQuota_AC04_ReadUsageIsolatedPerTenant`; contract `TestContractSTORE210_GetUsage`, `_ListAndGetTenants` | `internal/api/quota.go:70` `usageRoute` (no tenant parameter), `:90` `tenantsRoute`, `:117` `tenantRoute`; `internal/api/admin.go` `requireAdmin`; `internal/store/quota.go:266` `UsageAt` | GO-AC |
| §4, §5.3 quota edit with CAS | **AC-API-5** | `quota_acceptance_test.go:475` `TestQuota_AC05_QuotaEditCAS` (incl. 10 admins at once: exactly 1 winner); contract `TestContractSTORE210_QuotaCAS` | `internal/api/quota.go:221` `putQuotaRoute`, `:162` `requiredVersion` (428), `:209` `parseLimit`; `internal/store/quota.go:218` `SetQuota` | GO-AC |
| §2 default quota | **AC-API-6** | `quota_acceptance_test.go:526` `TestQuota_AC06_DefaultQuota`; `cmd/storecraft/quota_acceptance_test.go:392` `TestQuota_AC06_SeededTenantsGetTheDefaultQuota`; `:219` (tenants created by v1.0.0) | `internal/store/quota.go:116` `readQuota` (no row = default, version 0) | GO-AC |
| §5.1 step 4 replays are never counted or rejected | **AC-API-7** | `quota_acceptance_test.go:551` `TestQuota_AC07_IdempotencyNeverDoubleCounts` (incl. 20 identical requests at once) | `internal/store/apply.go` `Apply`: replay lookup before `execOp`; a rejected op rolls back (no record) | GO-AC |
| §1 survives restart | **AC-API-8** | `quota_acceptance_test.go:624` `TestQuota_AC08_SurvivesRestart` | persisted tables `tenant_quotas`, `tenant_usage`, `flags` (migration 6) | GO-AC |
| §3.1 canonical bytes, fixes F1 | **AC-API-9** | `quota_acceptance_test.go:652` `TestQuota_AC09_CanonicalStorageAndByteCount`; `internal/verify/quota_acceptance_test.go:182` `TestQuota_AC09_CanonicalBytesReplicateIdentically`; `internal/store/usage_test.go:51` `TestQuotaCanonicalJSON` | `internal/store/canonical.go:21` `CanonicalJSON`; `apply.go:134`; `internal/store/replica.go:82` (canonical on apply); `internal/api/errors.go:39` `marshal` (no HTML escaping) | GO-AC; mutation M2 |
| §8 replicas | **AC-API-10** | `internal/verify/quota_acceptance_test.go:73` `TestQuota_AC10_ReplicaServesQuotaStateAndVerifies` (100 writers, equal usage, flags and quotas, 409 on edits, verify OK); `:150` `TestQuota_AC10_VerifyReportsUsageAndQuotaDifferences` | `internal/store/replica.go:82` (subject `quota` rows); `internal/replica/replica.go:184` `pullOnce` (flags, `flags_version`); `internal/api/admin.go:71` `adminChangesRoute` (flags field, early answer); `internal/feed/feed.go:62` `PollAllUntil`; `internal/store/digest.go:59`; `internal/verify/verify.go:289` `compareSummaries` (usage_mismatch, quota_mismatch) | GO-AC |
| §10 feature flag | **AC-API-11** | `quota_acceptance_test.go:689` `TestQuota_AC11_FlagOffCountsButDoesNotReject`; `:714` `TestQuota_AC11_ToggleUnderConcurrentLoad`; contract `TestContractSTORE210_Flags` | `internal/store/quota.go:361` `Flag`, `:369` `SetFlag`; `checkQuota` reads the flag in the write transaction; `internal/api/quota.go:281` `flagsRoute`, `:307` `putFlagRoute` | GO-AC; mutation M3 |
| §3.2, plan R4 the old binary on the new schema | **AC-API-12** | `cmd/storecraft/quota_acceptance_test.go:219` `TestQuota_AC12_V1BinaryOnMigratedDatabase` (runs `releases/v1.0.0/storecraft.exe`); `internal/store/usage_test.go:80` `TestQuotaMigration6IsAdditive`, `:143` `TestQuotaUsageTriggersFollowEveryWrite` | migration 6 (additive; triggers count v1 writes too) | GO-AC |
| §3.2, plan R3 backfill | **AC-API-13** | `cmd/storecraft/quota_acceptance_test.go:346` `TestQuota_AC13_BackfillIsCorrectIdempotentAndResumable`; `:413` `TestQuota_BackfillDuringLiveWrites`; `internal/store/usage_test.go:180` `TestQuotaBackfillUsage` | `internal/store/backfill.go:85` `BackfillUsage`, `:31` `writeTxRetryBusy`; `cmd/storecraft/backfill.go:16` | GO-AC |
| §9 observability: metrics | (spec §9) | `quota_acceptance_test.go:775` `TestQuotaMetrics`; `internal/api/metrics_test.go` `TestMetricsLabelsUseRoutePatternNotRawPath` (tenant_id only on the two gauges) | `internal/metrics/storecraft.go:63` `AddQuotaGauges`; `internal/metrics/metrics.go` `NewFuncGauge`; `internal/api/metrics.go:80` | GO-AC |
| §9 observability: logs | (spec §9) | live smoke in DONE.md B7 (log lines shown) | `internal/api/quota.go` `putQuotaRoute`, `putFlagRoute` (`s.log.Info`, never tokens) | manual; DONE.md B7 |

## Console acceptance criteria (spec section 7)

| Requirement | AC | Playwright (`web/e2e/quota.spec.ts`) | Vitest | Code (`web/src/`) | Evidence |
|---|---|---|---|---|---|
| meter states | **AC-UI-1** | `:46` normal, `:60` loading/error/retry, `:85` unlimited, `:94` near (axe), `:102` over (axe) | `components/UsageMeter.test.ts`; `quota.test.ts` | `components/UsageMeter.vue`, `quota.ts` (`meterState`, `keysText`, `bytesText`) | PW, VT |
| live meter | **AC-UI-2** | `:111` (another client creates, overwrites, deletes; ≤ 2 s) | `composables/useUsage.test.ts` (debounce, poll, newest seq wins) | `composables/useUsage.ts`; `views/KeysView.vue` (feed `onChange` → `usage.changed()`) | PW, VT |
| over quota at save | **AC-UI-3** | `:133` (editor stays open, numbers, advice, axe), `:158` (code, not text; forbidden differs) | `views/KeysView.test.ts` "over quota at save" (2 tests); `quota.test.ts` `overQuotaMessage` | `views/KeysView.vue` `failureText`; `views/useKeys.ts` (details kept); `quota.ts` `overQuotaMessage` | PW, VT; mutation in DONE.md B8 |
| admin quota editor | **AC-UI-4** | `:192` (values, usage, version; invalid input sends nothing; If-Match) | `components/QuotaAdmin.test.ts` (TenantQuotaEditor) | `components/TenantQuotaEditor.vue`; `quota.ts` `parseLimitField` | PW, VT |
| editor conflict | **AC-UI-5** | `:233` (both sides, Tab trap, Esc, focus back to Save, Cancel), `:259` (Reload, Overwrite) | `QuotaAdmin.test.ts` (412 → dialog; Overwrite with the latest version) | `components/ConflictDialog.vue` (generalised: title, noun, intro); `TenantQuotaEditor.vue` `closeConflict` | PW, VT |
| flag hides the feature live | **AC-UI-6** | `:282` (tenant and admin consoles; ≤ 5 s, no reload) | `stores/flags.test.ts` | `stores/flags.ts` (the one place the flag is read); `App.vue` (polling); `views/AdminHomeView.vue`, `KeysView.vue` (`v-if="flags.quotaEnabled"`) | PW, VT |
| admin flag toggle | **AC-UI-7** | `:311` (confirmation names the effect; If-Match), `:327` (412 → reload) | `QuotaAdmin.test.ts` (QuotaEnforcementPanel, 2 tests) | `components/QuotaEnforcementPanel.vue` | PW, VT |
| keyboard and a11y | **AC-UI-8** | `:349` (keyboard-only editor, admin axe), `:364` (keys page axe); axe also inside `:94`, `:102`, `:133`, `:233` | `UsageMeter.test.ts` (role/aria) | all of the above; `BaseModal.vue` (focus trap) | PW |
| §9 client events | (spec §9) | — | `events.test.ts`; events asserted in `UsageMeter.test.ts`, `QuotaAdmin.test.ts` | `events.ts` (`track`); called from UsageMeter, KeysView, TenantQuotaEditor, QuotaEnforcementPanel | VT |

## Changed code that is not mapped to an AC

Listed so nothing is hidden. Each item is support or documentation, not new behaviour.

| Change | Why |
|---|---|
| `internal/store/backfill.go` `writeTxRetryBusy` | Plan R3 / plan.md finding: the backfill was starved by SQLite's busy handler. Serves AC-API-13 but is an operational fix. |
| `internal/feed/feed.go` `PollAllUntil` | Lets a flag change reach replicas at once (plan R7); supports AC-API-10/11. |
| `internal/metrics/metrics.go` `NewFuncGauge` | Mechanism for the spec §9 gauges (values read at scrape time). |
| `web/src/views/AdminHomeView.vue` | Removed the sentence "More admin tools will appear here in later versions" (it is no longer true). |
| `web/src/api/client.ts` | Unchanged since B3 (the stubs became real calls when the server implemented them). |
| Test-only changes to existing tests | Spec-driven updates in B7, recorded in DONE.md B7: metrics label rule, two verify tests, the whitespace test, the migration count. |
| Docs | `CLAUDE.md`, `web/CLAUDE.md`, `api/openapi.yaml` (`flags_version`), `docs/capstone/*`. |

## Added after B9

| Requirement | Test | Code | Evidence |
|---|---|---|---|
| §8 an un-upgraded v1.0.0 replica keeps replicating (the "replicas first" order) | `cmd/storecraft/release_test.go` `TestReleaseV1ReplicaKeepsReplicatingFromNewPrimary` (real v1.0.0 process) | quota rows are `kind='tenant'`; flags travel outside the log | `go test -race -count=1 -run TestReleaseV1Replica ./cmd/storecraft` |
| §9 alert rule | `internal/api/alert_test.go` `TestAlertRuleFiresInForcedScenario`, `TestQuotaRejectionCounterCountsOnlyQuotaRejections` | `docs/capstone/alert.md` (the rule is read from it) | `go test -run 'TestAlertRule\|TestQuotaRejectionCounter' ./internal/api` |
| INC-1 regression (B10/B11) | `internal/replica/replica_test.go` `TestRunStoppedBeforeItsFirstStepReturnsNil` | `internal/replica/replica.go` `Run` | `go test -run TestRunStoppedBeforeItsFirstStepReturnsNil ./internal/replica` |
| B14 SLO impact | `internal/api/bench_test.go` `BenchmarkPutQuotaOff` / `BenchmarkPutQuotaOn` | quota check in `execOp` | `go test -run XXX -bench BenchmarkPut -benchtime 3000x -count 3 ./internal/api` |
