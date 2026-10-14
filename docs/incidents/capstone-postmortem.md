# Postmortem: INC-1, CI red on a replica restart test

| | |
|---|---|
| Date | 2026-10-14 (detected 00:06:37, fixed 00:08:12, local +05:30) |
| Severity | Low: release pipeline blocked; no user impact |
| Status | Resolved; regression test in place |
| Timeline | `docs/capstone/incident-timeline.md` |
| Kind | Real, unplanned failure during autonomous work. Used as the **rehearsal** for the B11 game day. The blind game day chosen by the user is still to do (`docs/capstone/gameday-prep.md`), and this postmortem must be redone for it. |

## Summary

- **What happened:** after the B10 changes, the full local CI went red. One existing replication test, `TestVerifyConvergesAfterRestartMidStream`, reported that the replica's run loop returned an error when it was stopped.
- **Cause:** a cancelled context during the run loop's *first* step (reading `applied_seq`) was returned as a failure, while the same cancellation later in the loop was correctly treated as a normal stop.
- **Fix:** one condition, protected by a deterministic regression test.

## User impact

- **None.** FACT: no build containing B10 was released or deployed; the release candidate `v2.0.0-rc.1` predates it.
- **Potential impact, had it shipped:** INFERENCE. A replica stopped within its first read logs "replication stopped: applied seq: context canceled" and exits with code 1 instead of 0. A process supervisor could treat a normal stop as a crash. No data is affected, because the step only reads.

## Root cause

FACT, checked against the code and the regression test output:
- `internal/replica/replica.go` `Run` began with `if _, err := r.cfg.Store.AppliedSeq(ctx); err != nil { return err }`.
- When the context was cancelled during that read, the error (`applied seq: context canceled`) was returned to the caller.
- The loop below handles the same situation with `if ctx.Err() != nil { return nil }`. The two exits disagreed.

## Contributing factors

- **Timing-dependent test, no deterministic test of the edge.** INFERENCE for the timing; FACT for the missing test. The restart test stops the replica 150 ms after starting it, which reaches the edge only when that first read is slow. Before INC-1 no test called `Run` with an already-cancelled context.
- **A heavier CI.** INFERENCE. STORE-210 added many concurrency tests (100 writers, `-race`), and CI runs the Go packages in parallel. The test had passed in every earlier run, including the CI run 22 minutes before.
- **A test that prints secrets-shaped data on failure.** FACT. `cmd/storecraft/node_test.go` calls `seed`, which prints tokens to standard output. `go test` shows that output only when the package fails, so the problem stayed invisible until a failure.

## What went well

- The CI script stopped at the first failing stage and exited 1. The A7 fix that made failures impossible to miss worked as intended.
- The helper's assertion ("want nil") named the exact contract that was broken, which led straight to the cause.
- The fix was proven both ways: the regression test failed before the fix with the same message as CI, and passed after. The flaky test then passed 10/10.
- Nothing was released from the red tree.

## Action items

Each is written as a small issue in `docs/capstone/action-items.md`.

| # | Action | Owner |
|---|---|---|
| AI-1 | Run the blind B11 game day (the user chooses the fault) and redo this postmortem for it | project owner (you) |
| AI-2 | `seed` writes to a given writer; tests discard the token output. **Done in B10.** | Claude Code |
| AI-3 | Check every long-running loop for the same pattern (a cancelled context reported as an error on an early step) | Claude Code (B13 triage) |
| AI-4 | Add a CI job, or a documented manual step, that runs the timing-sensitive replica and verify tests with `-count=10` before a release | project owner |
| AI-5 | Validate `QuotaRejectionsHigh` with `promtool test rules` where Prometheus is available | operator of a real deployment |

## Claims that were INFERENCE and turned out wrong

All during STORE-210, recorded honestly:

1. **Plan Revision 2, item 7 (B4):** "BEGIN IMMEDIATE with busy_timeout serialises the backfill CLI and the server; no extra locking is needed." **Wrong in part.** `TestQuota_BackfillDuringLiveWrites` showed the backfill process failing with `SQLITE_BUSY` under a steady stream of server writes. SQLite's busy handler is not fair, and a retry with backoff was needed (plan.md "Findings").
2. **Migration 6 first draft (B6):** the assumption that `INSERT OR IGNORE` inside a trigger is always safe. **Wrong.** An UPSERT on `kv` overrides the conflict policy of statements inside its triggers, which raised `UNIQUE constraint failed`. Found by `TestQuotaUsageTriggersFollowEveryWrite`.
3. **Risk register R2 and R7 (B4):** the detection signals said `verify` would check usage against a scan and would compare flags. **Not what was built.** The design moved to comparing counters between nodes, and flags are checked by tests. The register was corrected in B7 and the corrections are marked there.
4. **For INC-1 itself:** the explanation "heavy CI load made the first read slow" is still INFERENCE. It is plausible and consistent with the evidence, but not measured, and it is not needed for the fix, which is deterministic.
