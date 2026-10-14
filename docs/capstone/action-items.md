# Action items from STORE-210 (INC-1 postmortem and delivery findings)

Each item is a small issue: title, why, owner, status.

## AI-1: Run the blind B11 game day
- **Why:** INC-1 was real but not blind; B11 asks for a fault chosen by the user and investigated from the symptom only.
- **Owner:** project owner (you).
- **Status:** open. The steps are in `docs/capstone/gameday-prep.md`.

## AI-2: Tests must not print tokens
- **Why:** a failing `cmd/storecraft` test printed three (synthetic) tenant tokens into the CI log through `seed`.
- **Owner:** Claude Code.
- **Status:** done in B10. `seed` writes to a given writer, `node_test.go` passes `io.Discard`, and the stored red log was redacted.

## AI-3: Audit loops for "cancellation reported as failure"
- **Why:** INC-1's root cause was an early step returning a cancelled context as an error.
- **Owner:** Claude Code.
- **Status:** done in B13. The independent review checked the replica `Run` loop, `PollAllUntil`, `Stream` and `writeTxRetryBusy` (all correct) and found one more instance, in `verify` (finding R3), which is fixed with a regression test (`review-triage.md`).

## AI-4: Repeat timing-sensitive tests before a release
- **Why:** the restart test reached the INC-1 edge only under load; one green run does not prove a timing-sensitive test is stable.
- **Owner:** project owner.
- **Status:** open. Suggested command: `go test -race -count=10 -run 'Verify|Replica|Restart' ./cmd/... ./internal/replica/... ./internal/verify/...` before `scripts/snapshot.ps1`.

## AI-5: Validate the alert rule with promtool
- **Why:** `QuotaRejectionsHigh` is tested with a small evaluator (alert.md, "Limitation"), not with Prometheus itself.
- **Owner:** operator of a deployment with Prometheus.
- **Status:** open.

## AI-6: Bundle budget headroom
- **Why:** the console is 115.4 of 120 kB; the next feature will hit the budget.
- **Owner:** project owner (decide: raise the budget, or split the admin page into a lazily loaded chunk).
- **Status:** open.

## AI-7: Contract step (tighten the schema) is scheduled, not executed
- **Why:** once no v1.0.0 binary can run against the database, `changes.subject` could get a CHECK constraint and `tenant_usage.tenant_id` a foreign key. Doing it now would break rollback to v1.0.0 (see release-readiness.md).
- **Owner:** project owner.
- **Status:** scheduled for the release after v2.0.0, when v1.0.0 rollback support ends.
