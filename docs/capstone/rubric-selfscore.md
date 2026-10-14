# Rubric self-score (STORE-210 capstone)

**Scale** (the rubric's own level descriptions are not in this repository, so this is the scale used here):
- **3:** complete, with evidence you can open or re-run
- **2:** present and real, with a named gap
- **1:** missing or not credible

These scores were written by Claude, the same agent that built the work. Treat them as a starting point for your own or your reviewer's judgement, not as a verdict.

| # | Dimension | Score | Evidence (open or run) |
|---|---|---|---|
| 1 | Evidence and verification | **3** | `docs/DONE.md`: one row per step with the exact command and its real result, including red runs. Full logs: `docs/capstone/ci-run-B9.log`, `ci-run-B10.log`, `ci-run-B10-red.log`, `ci-run-B13.log`, `red-output.txt`, `drill-log.md`. Every claim in the release notes names its test. FACT/INFERENCE/UNKNOWN labels in intake, incident and readiness docs. |
| 2 | Spec and traceability | **3** | `docs/capstone/spec.md` (13 API + 8 UI ACs, each with a pass condition; approved by your reviewer); `docs/capstone/trace.md` (every AC → tests → code file:line → evidence command, plus a list of unmapped changes); a script checks the trace.md line references. |
| 3 | Code quality and scope | **2** | Scope held to the spec (non-goals respected; unmapped changes listed in trace.md). Concurrency proven by mutation (DONE.md B7). **Gap:** the independent review still found 7 real defects after implementation, mostly async edge cases in the console plus a long backfill transaction (`review-triage.md`). All are fixed with regression tests, but they should have been caught earlier. The bundle is at 115.4 of 120 kB (AI-6). |
| 4 | Testing | **3** | Tests written first and shown failing for the right reason (`red-output.txt`). 100-writer concurrency tests under `go test -race -count=20 -run Quota ./internal/...`. Real HTTP and real temp SQLite. The real v1.0.0 binary in tests. Playwright against the real server with axe. Mutation checks M1–M3 and an AC-UI-3 mutation, each caught. Regression tests for INC-1 and for every reproduced review finding. **Minor gap:** the alert rule is tested with an evaluator, not with Prometheus (AI-5). |
| 5 | Git and PR hygiene (adapted: snapshots and docs) | **2** | Snapshots replace tags: `releases/v1.0.0`, `releases/v2.0.0-rc.1`, `releases/v2.0.0`, each with SHA256SUMS and a source.zip. Every change is logged in DONE.md with evidence. **Gap:** no per-change review record such as a PR. The rc.1 snapshot README says "CI skipped" (`-SkipCI`); the green CI it relied on is in `ci-run-B9.log`, but the snapshot itself does not prove it. |
| 6 | Independent review | **2** | A fresh AI context reviewed read-only; every finding was reproduced before action, 8 of 9 reproduced and fixed with failing-first tests, 1 not reproduced and recorded (`review-triage.md`). **Gap:** no human peer review yet; the playbook requires one (release-readiness go/no-go row 7). The B4 plan challenge was also raised by Claude standing in for you. |
| 7 | Release readiness | **3** | `docs/capstone/release-readiness.md` (measured SLO impact, rollout plan, go/no-go table, the one signal and why, why the contract step is scheduled); `scripts/rollback-drill.ps1` with a PASS in `drill-log.md`, including an extra v1.0.0 leg; the "replicas first" claim proven with a real v1.0.0 replica; `alert.md` with a runbook. **Open at release time:** go/no-go row 7 (human review) and the blind game day (AI-1). |

## Everything that scores below 3, and what would raise it

1. **Code quality (2):** seven defects were found only by the independent review. To raise it: put async-race and error-path cases on the console checklist before implementation; review the backfill's transaction size up front.
2. **Snapshots and docs (2):** no per-change review trail, and rc.1 says "CI skipped". To raise it: always snapshot without `-SkipCI` (v2.0.0 was built that way; see DONE.md B15), and add a short review note per step.
3. **Independent review (2):** no human reviewer yet. To raise it: a person reviews `review-triage.md` and the diff of `releases/v1.0.0/source.zip` against `releases/v2.0.0/source.zip`, and records approval.
4. **Not scored, but incomplete:** the blind game day (B11) and your retrospective ratings (B15) need you. `docs/capstone/gameday-prep.md` and `docs/retrospective-week2.md` say exactly what to do.
