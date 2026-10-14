# Incident INC-1: CI red on a replica restart test (B11 rehearsal)

**What kind of incident this is (honest label).**
- This is NOT the blind game day of step B11. That one needs the user to choose and apply a fault without telling Claude; see `gameday-prep.md`. It is still to do.
- INC-1 is a **real, unplanned failure** that happened while Claude worked autonomously: the full CI went red after step B10. Nobody injected a fault.
- It was handled with the B11 procedure: symptom first, mitigation, investigation with FACT / INFERENCE / UNKNOWN labels, timestamps from files and logs, and a regression test that fails without the fix.

## Symptom (as first seen)

`scripts/ci.ps1` stage 3 failed:

```text
--- FAIL: TestVerifyConvergesAfterRestartMidStream (6.12s)
    clustertest.go:177: replica Run returned applied seq: context canceled on pause, want nil
FAIL	storecraft/cmd/storecraft	73.725s
[FAIL] go test -race -count=1 ./... (144.1s)
FAIL: 2 of 3 stages passed, total 150.8s
```

(source: `docs/capstone/ci-run-B10-red.log`; 3 synthetic test tokens in that copy were redacted, see below)

## 1. Mitigation first

- **User impact: none.** FACT: no deployment runs this build; `releases/v2.0.0-rc.1` was snapshotted from the previous, green CI run.
- **Release mitigation:** do not snapshot or release from a red tree. FACT: no snapshot was made between the red run and the fix.
- **Production mitigation, had this shipped:** INFERENCE. The defect only affects a replica that is stopped within its first database read. The worst effect is a misleading error ("replication stopped: applied seq: context canceled") and exit code 1 on a normal shutdown. Restarting the replica is enough; no data is affected, because the failing step only reads.

## 2. Investigation (each claim labelled)

1. **FACT:** the message comes from `clustertest.(*Replica).Pause`, which requires `replica.Run` to return nil when its context is cancelled (`internal/clustertest/clustertest.go:177`).
2. **FACT:** `replica.Run` (`internal/replica/replica.go`) had two exits for a cancelled context. Inside the pull loop it returned nil (`if ctx.Err() != nil { return nil }`). But the first statement, `r.cfg.Store.AppliedSeq(ctx)`, returned its error unchanged. The text "applied seq: context canceled" is exactly that error (`store.AppliedSeq` wraps it as `applied seq: %w`).
3. **FACT:** so `Run` returns that error only if the context is cancelled while that first read is still running.
4. **INFERENCE:** in this test the replica is stopped 150 ms after it starts. For the bug to show, the first read must have taken longer than that. CI runs 9 test packages in parallel with `-race`, and opening a pooled SQLite connection on a loaded machine can plausibly take that long. Not measured. The same test had passed in every earlier run (including the B9 CI 22 minutes before).
5. **FACT:** B7 did not change `replica.Run`'s first step. INFERENCE: the B7–B10 changes (more tests, migration 6) made CI heavier, which made the timing window more likely to be hit.
6. **UNKNOWN:** how often this happened before; earlier CI logs show no such failure.

## 3. Timeline

All times are local (+05:30).

| Time | Event | Source |
|---|---|---|
| 2026-10-13 23:57:37 | B9 CI run green, 9/9 stages | mtime of `docs/capstone/ci-run-B9.log`; its last line `exit=0` |
| 2026-10-13 23:59:14 | `releases/v2.0.0-rc.1` snapshot written | mtime of `releases/v2.0.0-rc.1/SHA256SUMS.txt` |
| 2026-10-14 00:02:21 | Last B10 change before the run (`alert_test.go`, `alert.md`) | file mtimes |
| ≈ 00:04:07 | B10 CI run starts | completion time (next row) minus the reported total of 150.8 s |
| 2026-10-14 00:06:37 | CI ends red at stage 3 (symptom above) | mtime of the CI task's output file; `ci-run-B10-red.log` |
| 2026-10-14 00:07:50 | Regression test `TestRunStoppedBeforeItsFirstStepReturnsNil` written; it FAILS without the fix: `Run with a cancelled context returned applied seq: context canceled, want nil` | mtime of `internal/replica/replica_test.go`; output in DONE.md (B10) |
| 2026-10-14 00:08:12 | Fix in `replica.Run` (a cancelled context during the first step is a normal stop) | mtime of `internal/replica/replica.go` |
| after 00:08:12 | With the fix: regression test PASS; `internal/replica` PASS; `TestVerifyConvergesAfterRestartMidStream` PASS 10/10 with `-race -count=10` | command output in DONE.md (B10) |
| 2026-10-14 00:11:22 | `storecraft.exe` rebuilt; full CI re-run started | mtime of `storecraft.exe` |
| (see DONE.md B10) | Re-run result | `docs/capstone/ci-run-B10.log` |

## 4. Fix and regression test

- **Fix (`internal/replica/replica.go`, `Run`):** if the first `AppliedSeq` read fails *and* the context is cancelled, return nil, as the loop already does.
- **Regression test (`internal/replica/replica_test.go`, `TestRunStoppedBeforeItsFirstStepReturnsNil`):** it calls `Run` with an already-cancelled context, which deterministically reaches the faulty path without depending on timing. It **failed before the fix** (output above) and **passes after**.

## Side finding during the investigation

- **FACT:** the red log contained three tenant tokens. A failing package makes `go test` print the package's output, and `cmd/storecraft/node_test.go:110` calls `seed`, which prints the tokens it creates.
- They were synthetic, from a deleted temp database, but the rule "never put tokens in logs" applies. The stored copy was redacted (marked at its top), and `seed` was changed so tests discard that output (DONE.md B10).
