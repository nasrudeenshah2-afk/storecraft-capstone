# Retrospective, week 2 (STORE-210)

> **You need to fill this in.** Step B15 compares *your* self-ratings at the start, middle and end of the week, and only you have those numbers. Claude has not invented them. Replace every `?` and every *(your words)* below. The factual parts were filled from `docs/DONE.md` and can be checked there.

## 1. Self-ratings (1 = not confident, 5 = could teach it)

| Skill | Start of week | Middle (after B7) | End (after B15) | What changed it |
|---|---|---|---|---|
| Writing a spec with testable acceptance criteria | ? | ? | ? | *(your words)* |
| Contract-first API design (OpenAPI, error envelope, ETags) | ? | ? | ? | *(your words)* |
| Tests before code (red, then green) | ? | ? | ? | *(your words)* |
| Concurrency and transactions (races, CAS) | ? | ? | ? | *(your words)* |
| Safe schema changes (expand / contract, backfill) | ? | ? | ? | *(your words)* |
| Frontend states, errors and accessibility | ? | ? | ? | *(your words)* |
| Incidents, postmortems, rollback | ? | ? | ? | *(your words)* |
| Directing an AI coding assistant | ? | ? | ? | *(your words)* |

## 2. What happened (facts from DONE.md)

| Step | Outcome |
|---|---|
| B1 intake | Found finding F1: `verify` falsely reported divergence for `<`, `>`, `&`. Reproduced live. |
| B2–B3 spec and contract | 13 API and 8 UI acceptance criteria; spec approved by the reviewer; 7 new operations in `api/openapi.yaml`. |
| B4 plan | The "two requests at the same moment" challenge moved the quota check into the write transaction (Revision 2). |
| B5 red tests | 21 Go and 16 Playwright acceptance tests, all failing for the mapped reason. |
| B6 migration | Two real defects found by tests: a trigger conflict-policy bug, and the backfill starved by SQLite's busy handler. |
| B7 backend | All API ACs green; `-race -count=20` green; three mutations each caught by the right test. |
| B8 console | 119 unit and 31 browser tests green; a focus-return bug fixed. |
| B9 | CI 9/9; `v2.0.0-rc.1`. |
| B10 | Alert rule with a forced-scenario test; incident INC-1 (CI red) found and fixed with a regression test. |
| B13 | Independent review by a fresh AI context, then triage (see `review-triage.md`). |
| B14 | Rollback drill PASS, including a v1.0.0 leg. |

## 3. Went well (add yours)

- Writing the tests first made every later step measurable. FACT: each B5 failure turned green in B7/B8, without editing a test's intent.
- Concurrency claims were proven by mutation (moving the check outside the transaction: 67 successes instead of 10), not only argued.
- *(your words)*

## 4. Went badly or was hard (add yours)

- Several plan assumptions were wrong and only tests showed it (see the postmortem, "Claims that were INFERENCE and turned out wrong").
- Tool friction on Windows: escape sequences were silently turned into control characters three times (B3, B5, B14). This is now checked with a control-character scan.
- *(your words)*

## 5. Steps that needed a person and were not done by one

- B4: challenging the plan (Claude stood in for the reviewer; marked as such in plan.md).
- B11: the blind game day (`docs/capstone/gameday-prep.md`).
- B13: a human peer review. A fresh AI context reviewed instead; the playbook still requires a person.
- This retrospective's ratings.

## 6. Next week, I will…

- *(your words)*
