# Remaining prompts: A5 to B15 (plan, approve, implement, test at the end)

Put this file in your project as `docs/capstone/PROMPTS.md`. You work with Claude Code one step at a time:

1. You name a step ("do step A5").
2. Claude Code reads the code and shows a PLAN. It writes no code yet.
3. You read the plan. You reply `approved` or ask for changes.
4. Claude Code implements, then runs the automated checks itself and pastes the real output.
5. It reports what to test by hand LATER and stops.

Your manual testing happens at the end (see "FINAL TESTING"). The automated checks (go vet, go test, lint, typecheck, unit tests) still run after every step, because those are cheap and catch most mistakes early.

---

## 0. Kickoff prompt (paste this once at the start of each Claude Code session)

```text
Read docs/capstone/PROMPTS.md, CLAUDE.md, web/CLAUDE.md (if it exists) and docs/DONE.md.
We work through PROMPTS.md one step at a time. Work ONLY on the step I name. For every step follow this protocol:

1. PLAN: read the relevant code first. Show a plan: files to create or change, key decisions, risks, and which tests you will write. Write NO code. Then STOP and wait for me to reply "approved" or give changes.
2. IMPLEMENT: only after approval. Keep the change small and in scope. Follow CLAUDE.md and web/CLAUDE.md.
3. AUTOMATED CHECKS: run the checks listed for the step and paste the REAL output. If something fails, fix the cause and rerun. Never weaken, delete or skip a test to make it pass.
4. REPORT: list the files changed, add anything I should check by hand to docs/capstone/manual-test-todo.md (with the exact command), and give me the exact row to add to docs/DONE.md. Then STOP. Do not start the next step.

Label claims FACT (you ran or read it), INFERENCE (you believe it) or UNKNOWN. Do not create a Git repository or a Makefile. Do not add a dependency that is not on the approved list in CLAUDE.md or web/CLAUDE.md without asking me.

Confirm you understood, then wait for my first step name.
```

To start a step later, just say: `Do step A5.` and, after reading the plan, `approved` (or your corrections).

## Standing rules for every step

- Approved Go dependency: `modernc.org/sqlite` only. Standard library for everything else.
- No Git, no Makefile. Snapshots replace tags (see A8).
- Synthetic data only. Never real secrets.
- Go checks after any Go change: `go build ./...`, `go vet ./...`, `gofmt -l .` (prints nothing), `go test -race -count=1 ./...`.
- Web checks after any web change: `npm run lint`, `npm run typecheck`, `npm run test`, `npm run build` (from `web/`).
- Windows PowerShell. Scripts are `.ps1`.
- You must stop and rebuild the Go program (`go build -o storecraft.exe ./cmd/storecraft`) after Go changes. Claude Code cannot do this for servers you started yourself.

## Product-owner decisions (already answered, used by B1 and B2)

| Question | Answer |
|---|---|
| What counts as usage? | Key count and total bytes of values (UTF-8 length). Key names do not count. |
| Default quota | 1000 keys and 1 MiB per tenant. `null` means unlimited. |
| Over-quota response | HTTP 403, code `quota_exceeded`, message includes used and limit. Not 429, because retrying will not help. |
| Overwrite | Counts only the byte difference. A write that shrinks a value is always allowed. |
| Delete | Frees quota. |
| Admin lowers a quota below current usage | Allowed. The tenant is then over quota: writes that grow usage are rejected; deletes and shrinking overwrites are allowed. |
| Who can edit quotas | Admin token only. Quota records have a `version` and use If-Match (CAS). |
| Who can read usage | A tenant reads its own via `GET /v1/usage`. Admin reads any tenant. |
| Feature flag | `quota_enforcement`, runtime-toggleable by admin with no restart. Off means no rejections and the UI hides the quota feature. |

---

# PHASE A: finish the foundation

(A1 to A4 are done: scaffold, SQLite store, tenants, auth, versioned CAS API, change feed, SSE, idempotency, metrics, OpenAPI, replica, `verify`. The A4 manual pause test is in FINAL TESTING.)

## A5. Vue console, part 1: foundation and key management

```text
STEP A5. Plan first, then wait for my approval.

Context: The Go backend is complete (keys, changes, SSE, replica, metrics, api/openapi.yaml). Create the Vue console in web/. Extend the project; do not touch backend behaviour.

Task:
1. Create web/CLAUDE.md FIRST. Rules: Vue 3 Composition API with <script setup lang="ts">, typed props and emits, Pinia for shared state, strict TypeScript, no `any`, no @ts-ignore, no disabled lint rules, no Options API, no other state library, one styling approach (plain CSS in components with a small shared stylesheet). API types come ONLY from api/openapi.yaml. Approved dependencies: vue, pinia, vite, @vitejs/plugin-vue, typescript, vue-tsc, eslint, eslint-plugin-vue, typescript-eslint, vitest, @vue/test-utils, jsdom, @playwright/test, openapi-typescript, @axe-core/playwright. Ask me before adding anything else.
2. Scaffold Vite + Vue 3 + TypeScript (strict). Use a Vite dev proxy for /v1 and /metrics to http://localhost:8090 so no CORS code is needed in the Go server.
3. Generate types from api/openapi.yaml with openapi-typescript (npm script `gen:api`) into src/api/schema.d.ts. Write a small typed fetch client (src/api/client.ts): bearer token header, JSON handling, an ApiError class built from the standard error envelope ({"error":{"code","message"}}) with status, code, message; ETag read from responses; If-Match and Idempotency-Key support on writes.
4. Shared components in src/components: BaseButton, BaseTextField, BaseModal, ToastHost (with a useToast composable). Use them everywhere.
5. Pinia session store: token and role ("tenant" or "admin"). Keep it in sessionStorage so a refresh keeps you logged in; never put the token in a URL.
6. Views: LoginView (paste a token, choose Tenant or Admin) and KeysView (list with prefix filter, create, edit and delete a key). Edits send If-Match with the version shown. Map every API error (401, 404, 412, 409, 403 and so on) to a clear message using error.code, never string guessing.
7. Keep the bundle small and the app accessible (labels on inputs, focus handling in the modal, keyboard usable).

Constraints: types from the OpenAPI file only; no `any`; real behaviour must be visible in the UI, not hidden behind mocks.
Automated checks to run and show: `npm run lint`, `npm run typecheck`, `npm run test` (unit tests for the client's error mapping, the session store, and the KeysView logic using a stubbed fetch), `npm run build`.
Stop after the report. Add to manual-test-todo.md: open the app against a running primary and create, edit and delete a key.
```

## A6. Vue console, part 2: live updates, conflicts, browser tests

```text
STEP A6. Plan first, then wait for my approval.

Context: A5 is done. Extend web/; keep following web/CLAUDE.md.

Task:
1. Composable useChangeFeed: connect to GET /v1/changes/stream using fetch with a streaming reader (NOT EventSource, because EventSource cannot send an Authorization header and the token must never go in the URL). Parse SSE events (id, event, data), reconnect automatically from the last seen id (send Last-Event-ID), back off on errors, and fall back to long-poll GET /v1/changes?since=N if streaming fails. Expose reactive status (connecting, live, reconnecting, fallback) and the latest changes. Clean up on unmount.
2. KeysView updates live when another client changes a key (put, overwrite, delete) and shows a small "live" indicator. Apply changes by version so an old change never overwrites a newer value.
3. CAS conflict handling: when a save returns 412, open a dialog showing "your value", "latest value" and the choices Overwrite (re-send with the latest version), Reload, Cancel.
4. For the Admin role only: a replication status badge reading GET /v1/replication/status (role, applied_seq, primary_seq, lag).
5. Playwright (chromium only) end-to-end tests against the REAL Go server: propose in the plan how the test setup builds and starts a temporary primary with a fresh database and synthetic tenants, and stops it afterwards. Tests: login; create/edit/delete; live update from a second client; conflict dialog; reconnect after the stream is dropped. Add @axe-core/playwright checks on the login and keys pages (no violations).
6. A bundle budget check (npm script `check:bundle`) that fails if the main JS exceeds a size you choose, document it in web/CLAUDE.md.

Automated checks to run and show: lint, typecheck, unit tests, `npm run build`, `npm run check:bundle`, and the Playwright suite.
Stop after the report. Add to manual-test-todo.md: open two browser windows, change a key in one, watch it update in the other.
```

## A7. Local CI script (and Docker, optional)

```text
STEP A7. Plan first, then wait for my approval.

Task: Create scripts/ci.ps1 that runs in order and stops at the first failure: gofmt -l (must print nothing), go vet ./..., go test -race -count=1 ./..., then in web/: npm run lint, typecheck, test, build, check:bundle, and the Playwright suite. Print a clear PASS or FAIL for each stage and a final summary with the total time. Exit non-zero on failure.
If `docker --version` works on this machine, also add a Dockerfile for the Go binary and a docker-compose.yml with primary, replica and a static server for web/dist. If Docker is not installed, skip Docker entirely and say so; do not write untested Docker files.

Automated check: run `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1` and show the full output.
Stop after the report.
```

## A8. Snapshot script, demo scripts, v1.0.0

```text
STEP A8. Plan first, then wait for my approval.

Task:
1. scripts/snapshot.ps1 <name>: builds the Go binary and web/dist and copies them, plus a zip of the source (excluding node_modules and the databases), into releases/<name>/ together with a SHA-256 list and a README stating the Go and Node versions.
2. scripts/demo-up.ps1: builds the binary, deletes old demo databases, seeds 3 synthetic tenants into demo.db (saving the printed tokens to demo-tokens.txt), starts the primary on :8090 and the replica on :8091 as separate background processes, records their process IDs in a file, and prints the tokens and URLs. scripts/demo-down.ps1: stops exactly the processes it started (from the recorded IDs), nothing else, and confirms the ports are free.
3. Run scripts/snapshot.ps1 v1.0.0.

Constraints: Do not create a Git repository. Never kill a process by name only; use the recorded IDs.
Automated checks: run snapshot.ps1 v1.0.0, list releases/v1.0.0, start the snapshot's binary briefly and check /metrics answers 401 without a token (then stop it), and show the output.
Stop after the report. Add to manual-test-todo.md: demo-up, open the console, demo-down.
```

---

# PHASE B: the capstone (STORE-210)

Ticket: per-tenant storage quotas, usage metering and console. API: each tenant has a configurable maximum key count and maximum total value bytes; writes that would exceed quota are rejected in the standard error envelope (the spec decides the status code); a tenant can read its own usage; admins can set quotas. Usage must be correct under concurrency, survive restarts, replicate to replicas and be visible in metrics. Console: live usage meter per tenant, an admin quota editor that handles CAS conflicts, and a clear over-quota experience at the moment a save is rejected.

## B1. Ticket intake

```text
STEP B1. Plan first, then wait for my approval.

Context: storecraft v1.0.0 is snapshotted. Extend it; do not rebuild.

Task: Read the ticket above (docs/capstone/PROMPTS.md, the STORE-210 paragraph) and the code. Use a read-only subagent to find every Go package and Vue component the ticket touches. Write docs/capstone/intake.md with: touched files and packages with paths, assumptions, UNKNOWNs, questions asked, and the answers (use the product-owner table in PROMPTS.md). Every claim is FACT, INFERENCE or UNKNOWN.

Constraints: Read the code file by file; do not guess or sample. List every real instance before suggesting any change. Do not change any code in this step.
Automated check: none beyond confirming the file exists. 
Stop after the report.
```

## B2. Spec (needs your reviewer's approval)

```text
STEP B2. Plan first, then wait for my approval.

Task: Propose the schema, endpoints and error shapes FIRST and wait for my approval of that proposal. Then write docs/capstone/spec.md: problem, numbered acceptance criteria (at least 4 API ACs and 4 UI ACs; UI ACs cover states, errors and accessibility), non-goals, behaviour on replicas, observability requirements (metrics, client events, one alert rule), and the feature-flag behaviour. Every AC must be testable (a test can pass or fail it).
Constraints: no implementation code in this step.
Stop after the report.
```

**YOU:** give `spec.md` to your human reviewer and get approval BEFORE step B3. Do not skip this.

## B3. Contract first

```text
STEP B3. Plan first, then wait for my approval.

Task: Update api/openapi.yaml FIRST for: GET /v1/usage, admin endpoints to read and set a tenant's quota with If-Match (CAS), GET /v1/admin/tenants (so the console admin can choose a tenant), the quota_exceeded 403 error with used and limit, and the flag endpoints. Regenerate the TypeScript types (npm run gen:api). Review status codes, error envelope, headers and ETags in the plan before changing the file.
Constraints: no `any`, no @ts-ignore. Types come only from the spec. Do not implement the endpoints yet.
Automated checks: the Go OpenAPI route-coverage test is allowed to fail ONLY for routes not implemented yet; add contract tests that fail until the server implements them; `npm run typecheck` passes against a stub of the new client calls. Show real output and explain each expected failure.
Stop after the report.
```

## B4. Plan and risk register

```text
STEP B4. Switch to Plan Mode. Then wait for my approval.

Task: Write docs/capstone/plan.md: break the work into independently shippable steps in two streams, backend first and then frontend. Also write a risk register (likelihood, impact, mitigation, detection signal) with at least 6 risks across API, data and UI, for example: check-then-write race on usage, usage drift on overwrite, backfill double-run, the v1.0.0 binary breaking on the new schema, replica lag showing wrong usage, a stale usage meter in the UI, flag state differing between primary and replica.
Constraints: do not implement. I will ask you to revise the plan at least once. Keep both versions in the file (a "Revision 1" and "Revision 2" section) and say what changed and why.
Stop after the report.
```

**YOU:** challenge the plan at least once (for example: "what happens if two requests arrive at the same moment?") so the revision is real.

## B5. Acceptance tests first (they must FAIL)

```text
STEP B5. Plan first, then wait for my approval.

Task: Write Go tests for every API acceptance criterion and Vitest or Playwright tests for every UI acceptance criterion BEFORE any implementation. Run them and show each one fails for the EXPECTED reason (not a typo or compile error). Save the red output in docs/capstone/red-output.txt, mapping each failure to its AC.
Constraints: real HTTP and real concurrency (100 concurrent writers near the limit) with real temp SQLite files; no mocks that would hide the bug. It is expected that the full CI is red after this step; say so.
Automated checks: go build ./... and go vet ./... pass (the tests compile); the new tests FAIL for the mapped reasons; the old tests still pass.
Stop after the report.
```

## B6. Expand migration and backfill

```text
STEP B6. Plan first, then wait for my approval.

Task: Add a versioned, additive migration: tenant_quotas (tenant_id, max_keys, max_bytes, version) and tenant_usage (tenant_id, key_count, byte_count). Nothing is dropped, renamed or tightened. Add `storecraft backfill-usage` that computes usage from kv, is idempotent and resumable (safe to run twice, safe to stop and rerun). Decide in the plan how the backfill and live writes avoid racing each other.
Constraints: the releases/v1.0.0 binary must still run correctly against a migrated database. Additive only.
Automated checks: tests for the migration, backfill against a direct COUNT/SUM query, running it twice changes nothing, resuming after an interruption; go build, vet, gofmt, go test -race -count=1 for the migration and backfill tests.
Stop after the report. Add to manual-test-todo.md: run the v1.0.0 binary against a migrated demo.db.
```

## B7. Backend implementation (the hard one)

```text
STEP B7. Plan first, then wait for my approval. In the plan, explain exactly how the quota check, the write and the usage update stay in ONE SQLite transaction (for example an immediate transaction), how overwrite and delete adjust usage, and how replicas get quotas and usage (for example: replicas recompute usage while applying puts and deletes, and quota changes travel as a new change type).

Task: Implement the API acceptance criteria:
- Writes that would exceed the quota return 403 quota_exceeded in the standard envelope with used and limit; nothing is written.
- GET /v1/usage for a tenant; admin endpoints to read and set a tenant's quota with If-Match (412 on conflict); GET /v1/admin/tenants.
- Quota and usage changes flow through the change feed so replicas replicate them, and verify/digest also compare usage.
- Metrics: storecraft_tenant_usage_keys and storecraft_tenant_usage_bytes per tenant (labels bounded by the number of tenants), storecraft_quota_rejections_total, storecraft_quota_enforcement_enabled (0 or 1).
- Enforcement is controlled by the runtime flag `quota_enforcement` (the flag mechanism itself is finished in B10; for now read it from one place so B10 can plug in).

Constraints: real concurrency tests: 100 concurrent writers near the limit; the invariant "usage never exceeds the limit and always equals the real data" must hold across `go test -race -count=20`. Idempotent retries must not double count.
Automated checks: go build, vet, gofmt, `go test -race -count=1 ./...`, and `go test -race -count=20 -run Quota ./internal/...`; all API acceptance tests from B5 now pass; a primary+replica test shows usage digests equal after load.
Stop after the report.
```

## B8. Console feature UI

```text
STEP B8. Plan first, then wait for my approval.

Context: work in web/ and follow web/CLAUDE.md. The backend from B7 is done.

Task: Build, using the existing components and useChangeFeed: 
- a live usage meter per tenant (keys and bytes, used versus limit, with an accessible text alternative, not colour alone);
- an admin quota editor (admin chooses a tenant, edits max keys and bytes) that sends If-Match and handles a 412 with the existing conflict dialog;
- a clear over-quota experience at the moment a save is rejected: a message with used and limit, what the user can do (delete keys or ask an admin), and no lost input;
- the whole quota feature hides when the quota_enforcement flag is off (the flag wiring is finished in B10; read it from one place now).

Constraints: map errors from the envelope code (quota_exceeded, precondition_failed), never from message text. No new libraries.
Automated checks: lint, typecheck, unit tests, build, check:bundle, and the Playwright suite including the UI acceptance tests from B5 and axe with no violations.
Stop after the report.
```

## B9. Integrate and complete the trace table

```text
STEP B9. Plan first, then wait for my approval.

Task: Integrate backend and frontend in the project root. If anything conflicts, explain what each side intended BEFORE proposing a resolution; I will choose. Create docs/capstone/trace.md with requirement -> AC -> test (Go / Vitest / Playwright) -> code location -> evidence command for EVERY acceptance criterion. List any changed code that is not mapped to an AC.
Automated check: `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1` is fully green. Show the output.
Then run scripts/snapshot.ps1 v2.0.0-rc.1.
Stop after the report.
```

## B10. One flag and observability

```text
STEP B10. Plan first, then wait for my approval. In the plan, say where the flag state lives (recommended: persisted on the primary and replicated, so replicas and the UI agree) and how every request reads it without a restart.

Task: Put API enforcement and the UI behind the single runtime flag `quota_enforcement`. Admin can toggle it through an admin endpoint with no restart; the console reads the flag (and updates live when it changes) and hides or shows the quota feature without a page reload. Add the server metrics, client events (for example quota_rejected_shown, quota_editor_conflict) and one alert rule from the spec; put the alert rule in docs/capstone/alert.md with the condition, the threshold and why.
Constraints: Do not break the idempotency or CAS behaviour.
Automated checks: a Go test that runs a concurrent load while toggling the flag several times through the admin API and asserts ZERO failed requests other than expected quota_exceeded; a Playwright test that the UI hides and shows the quota feature when the flag changes, without a reload; a test that proves the alert condition triggers in a forced scenario; full CI green.
Then run scripts/snapshot.ps1 v2.0.0-rc.1 again only if it changed behaviour (say so), otherwise leave it.
Stop after the report.
```

## B11. Incident game day (YOU choose the fault)

Pick ONE fault yourself and apply it; do not tell Claude Code which one:

- run `storecraft backfill-usage` twice during a load run,
- stop the replica during writes, then restart it,
- change the system clock or a stored timestamp on the replica,
- make the console stop reconnecting (for example, stop the primary and restart it).

Then give Claude Code ONLY the symptom you see:

```text
STEP B11. We have an incident. Symptom only: [describe what you see, for example "usage in the console for tenant X is higher than the number of keys listed, and the replica shows a lower usage than the primary"].
Do not assume a cause. Plan first, then wait for my approval.

Task: 1) Tell me how to mitigate first (for example turning the flag off). 2) Investigate using logs, metrics and code; state FACT / INFERENCE / UNKNOWN for each claim. 3) Write docs/capstone/incident-timeline.md with timestamps taken from logs and metrics, not memory. 4) Propose the fix and a regression test that fails without the fix. Do not apply the fix until I approve.
Automated checks after the fix: the new regression test fails without the fix and passes with it (show both), then full CI green.
Stop after the report.
```

## B12. Blameless postmortem

```text
STEP B12. Plan first, then wait for my approval.

Task: Write docs/incidents/capstone-postmortem.md: summary, user impact, timeline, root cause, contributing factors, what went well, action items with owners, and which of your claims during the incident were INFERENCE that turned out wrong. Write docs/capstone/action-items.md listing every action item as a small issue (title, why, owner).
Constraints: cross-check every claim in the writeup against the real code and test output; do not summarise from memory. Blameless wording: describe systems and decisions, not people.
Stop after the report.
```

## B13. Independent review (start a NEW Claude Code session so it has no memory of building it)

```text
STEP B13. You are a reviewer and security auditor. You have not seen how this was built. Read-only: do not change any file.

Task: Review the STORE-210 changes on both backend and frontend against docs/capstone/spec.md and trace.md. For each finding give: file and line, severity, a way to reproduce it (a command, test or request), and label anything you cannot reproduce as INFERENCE. Look especially for: check-then-write races, usage drift on overwrite and delete, quota bypass through idempotent replays, tenant isolation on usage/quota/changes endpoints, admin-only endpoints reachable with tenant tokens, tokens in URLs or logs, replica divergence, and a11y or error-handling gaps in the UI.
Output a table.
```

Back in the main session:

```text
STEP B13 (triage). Here is the reviewer's table: [paste]. Plan first. For each finding: reproduce it yourself (a failing test or command output) before acting; mark unreproduced ones INFERENCE. Write docs/capstone/review-triage.md (finding, reproduced yes/no, action, evidence). Fix only reproduced findings, each with a test that fails without the fix. Nothing is silently ignored.
Automated check: full CI green.
```

**YOU:** also get one human peer to review and approve (the playbook requires it).

## B14. Release readiness and rollback drill

```text
STEP B14. Plan first, then wait for my approval.

Task: Write docs/capstone/release-readiness.md: SLO impact, rollout plan, go/no-go criteria, and the ONE signal to watch after release (for example the rate of storecraft_quota_rejections_total, or lag), with why. Explain why the "contract" step (tightening or removing old schema) is scheduled, not executed. Then write scripts/rollback-drill.ps1 that: starts the stack from the current build, writes data, runs `storecraft verify`, stops it, starts the stack from releases/v2.0.0-rc.1 on the SAME data, runs verify and a smoke check (login, read a key, write a key, read usage), stops it, starts the current build again, runs verify and the smoke check again, and writes docs/capstone/drill-log.md with the real outputs.
Constraints: nothing destructive without my approval; use the recorded process IDs from demo-up/demo-down; synthetic data only.
Automated checks: run the drill script and show docs/capstone/drill-log.md (verify outputs and smoke results after rollback and after roll-forward).
Stop after the report.
```

## B15. Release v2.0.0, demo notes, retrospective

```text
STEP B15. Plan first, then wait for my approval.

Task: Run scripts/snapshot.ps1 v2.0.0. Write releases/v2.0.0/RELEASE_NOTES.md verified against the real code and tests (cross-check each claim). Write docs/demo-script.md: a 10-minute demo of the quota feature in the console (what to click, what to say, which numbers to point at). Write docs/retrospective-week2.md comparing my self-ratings at the start, middle and end (I will supply the numbers). Write docs/capstone/rubric-selfscore.md: score all 7 rubric dimensions (Evidence and verification, Spec and traceability, Code quality and scope, Testing, Git and PR hygiene (adapted: snapshots and docs), Independent review, Release readiness) from 1 to 3 with a link to evidence for each, and an honest list of anything that scores below 3.
Automated check: full CI green on the final tree, and list releases/v2.0.0.
Stop after the report.
```

---

# FINAL TESTING (you do this at the end)

Claude Code has been collecting items in `docs/capstone/manual-test-todo.md`. Work through those and these checks. Use `scripts/demo-up.ps1` to start everything cleanly and `scripts/demo-down.ps1` to stop it.

1. **Automated first:** `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1` must be fully green.
2. **Replica (A4):** write keys on the primary, stop the replica, write 20 more, start the replica, and run `.\storecraft.exe verify -primary=http://localhost:8090 -replica=http://localhost:8091`. Expect `OK: zero divergence` and exit code 0. Also confirm a write to the replica returns 409 `read_only_replica`.
3. **Console (A5/A6):** log in as a tenant, create, edit and delete a key; open a second browser window and see changes appear live; edit the same key in both windows and see the conflict dialog.
4. **Quota API (B7):** set a small quota as admin, fill it, and confirm the next write returns 403 `quota_exceeded` with used and limit; delete a key and confirm writes work again. Run `go test -race -count=20 -run Quota ./internal/...`.
5. **Quota UI (B8):** usage meter updates live; admin quota editor shows the conflict dialog when two admins edit; the over-quota message appears at the moment a save is rejected.
6. **Flag (B10):** toggle `quota_enforcement` as admin while the console is open; the quota feature disappears and reappears without a reload; no requests fail.
7. **Metrics:** with the admin token, check `/metrics` for the usage gauges, `storecraft_quota_rejections_total` and the flag gauge.
8. **Migration safety (B6):** run the `releases/v1.0.0` binary against the migrated database.
9. **Rollback drill (B14):** read `docs/capstone/drill-log.md`; every verify says zero divergence and every smoke check passes.
10. **Rubric (B15):** read `rubric-selfscore.md` and check each evidence link opens a real file or command output.

# What to hand your reviewer

The whole project folder (or a zip of `releases/v2.0.0` plus `docs/`). Tell them: the foundation was built fresh, and Git tags, PRs and worktrees were replaced by snapshots in `releases/` and docs. Ask whether that is acceptable for grading.
