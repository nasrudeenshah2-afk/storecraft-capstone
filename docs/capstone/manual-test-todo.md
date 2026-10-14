# Manual tests to do later

Collected by each step; work through them during FINAL TESTING. Tick each box when done and note anything odd.

## A5 – Vue console, part 1 (keys)

Setup (PowerShell, two terminals):

```powershell
# Terminal 1 – project root: build, seed a fresh demo database, start the primary on :8090
go build -o storecraft.exe ./cmd/storecraft
.\storecraft.exe seed -db a5-demo.db          # prints 3 tenant tokens; copy one
$env:STORECRAFT_ADMIN_TOKEN = "a5-admin-token" # any synthetic value
.\storecraft.exe serve -addr :8090 -db a5-demo.db

# Terminal 2 – the console
cd web
npm run dev                                    # then open http://localhost:5173
```

(If another storecraft server already uses port 8090, stop it first or the `serve` command will fail.)

- [ ] Sign in as **Tenant** with a seeded token: the Keys page appears.
- [ ] **Create** key `app/theme` with value `"dark"`: it appears in the list with version 1 and a "Created" message.
- [ ] Try to create `app/theme` again: the dialog says it already exists, and the value is not overwritten.
- [ ] Type `dark` (no quotes) as a value: the value field shows a JSON error and nothing is sent.
- [ ] **Edit** `app/theme` to `"light"`: the dialog says "Editing version 1"; after saving the list shows version 2.
- [ ] **Conflict**: open Edit on `app/theme`, then in Terminal 3 change it behind the console's back:
      `curl.exe -X PUT -H "Authorization: Bearer <token>" -d '{\"value\":\"other\"}' http://localhost:8090/v1/keys/app/theme`
      Now press Save in the console: you see "Someone else changed ..." and your typed text is still in the box.
- [ ] **Delete** `app/theme`: a confirmation dialog appears; after confirming, the key is gone.
- [ ] **Prefix filter**: create `app/a`, `app/b`, `other`; filter by `app/` shows only the two `app/` keys; "Clear filter" shows all.
- [ ] **Refresh** the browser tab: you are still signed in. Close the tab and open a new one: you are signed out (sessionStorage).
- [ ] The token never appears in the address bar or in the browser's Network tab URLs (only in the Authorization header).
- [ ] Sign in with the **admin** token but choose **Tenant**: a clear "This is an admin token" message, not signed in.
- [ ] Sign in as **Admin** with the admin token: the admin page explains that keys need a tenant token.
- [ ] **Keyboard only** (no mouse): Tab reaches every button and field; Enter submits forms; in a dialog Tab stays inside it, Esc closes it and focus returns to the button that opened it.
- [ ] Stop the Go server while signed in and try to save: the console says it could not reach the server (no crash, input kept).

## A6 – Vue console, part 2 (live updates, conflicts, admin badge)

First: **restart your Go server** so it runs the rebuilt `storecraft.exe` (A6 added the `seq` field to `GET /v1/keys`; a server started before A6 does not send it, and live updates need it).

```powershell
# Terminal 1 – project root (stop any older storecraft server first)
$env:STORECRAFT_ADMIN_TOKEN = "a6-admin-token"
.\storecraft.exe serve -addr :8090 -db a5-demo.db

# Terminal 2
cd web
npm run dev        # open http://localhost:5173 in TWO browser windows (or one normal + one private window)
```

- [ ] Sign in as the same tenant in both windows. Both show a green **Live** indicator next to "Keys".
- [ ] Create a key in window 1: it appears in window 2 within a second, without reloading.
- [ ] Edit it in window 2: window 1 shows the new value and version. Delete it in window 1: it disappears from window 2.
- [ ] **Conflict**: open Edit on the same key in both windows. Save in window 1. Then save in window 2: a "Conflict" dialog shows "Your value" and "Latest value (version N)".
      Try **Cancel** (your text is still in the editor), **Reload latest** (the editor shows the other window's value and the new version) and **Overwrite with my value** (your value wins; window 1 updates live).
- [ ] While the editor is open in one window, change the key in the other: the editor shows "Someone changed ... (it is now version N)".
- [ ] **Reconnect**: with the console open, stop the Go server (Ctrl+C in Terminal 1). The indicator turns to **Reconnecting…**. Start the server again: it returns to **Live**, and a change made now still appears.
- [ ] Sign in as **Admin**: the header shows the replication badge (Primary, applied/primary seq, "lag 0 (in sync)"). Write a key as a tenant elsewhere; within ~5 s the applied seq goes up.
- [ ] Optional, replica: start a replica (`-role=replica -primary=http://localhost:8090 -addr :8091`, see root CLAUDE.md), run the console against it with `$env:STORECRAFT_API_TARGET="http://localhost:8091"; npm run dev`, sign in as admin with the replica's admin token: the badge says **Replica** with its lag.
- [ ] Keyboard only: Tab into the conflict dialog, choose each button with Enter, Esc closes it and focus returns to the editor.

## A7 – local CI and Docker

- [ ] Run the full CI yourself: `powershell -ExecutionPolicy Bypass -File scripts/ci.ps1`. Expect 9 stages, each `[PASS]`, ending with `PASS: 9 of 9 stages passed` and exit code 0 (`echo $LASTEXITCODE`). Takes about 3–6 minutes.
- [ ] Docker stack (Docker Desktop must be running). In PowerShell from the project root, using ports that do not clash with your own server:
      ```powershell
      $env:STORECRAFT_ADMIN_TOKEN = "docker-demo-admin"   # synthetic
      $env:PRIMARY_PORT = 18090; $env:REPLICA_PORT = 18091; $env:WEB_PORT = 18080
      docker compose up -d --build
      docker compose exec primary storecraft seed -db /data/storecraft.db   # copy a token
      ```
      Open http://localhost:18080, sign in with the token, create a key (it should be live), then check the replica:
      `curl.exe -H "Authorization: Bearer <token>" http://localhost:18091/v1/keys`
      and run `.\storecraft.exe verify -primary http://localhost:18090 -replica http://localhost:18091` (expect `OK: zero divergence`, exit 0).
      Finish with `docker compose down -v`.

## A8 – demo scripts and the v1.0.0 snapshot

Before you start: stop your own storecraft servers on :8090/:8091 (demo-up refuses to start while those ports are busy, and never stops other programs itself). Your old `storecraft.exe~` (left over from a rebuild while the server was running) can be deleted after that.

- [ ] `powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1` – it prints the URLs and tokens and creates `demo-tokens.txt` and `demo-pids.json`.
- [ ] Open the console URL it prints (http://127.0.0.1:5173), sign in with a tenant token from `demo-tokens.txt`, create a key, see it live; sign in as admin with the `admin:` token and see the replication badge (Primary, lag 0).
- [ ] Run `.\storecraft.exe verify -primary=http://127.0.0.1:8090 -replica=http://127.0.0.1:8091` with `$env:STORECRAFT_ADMIN_TOKEN` set to the admin token: `OK: zero divergence`.
- [ ] Run `demo-up.ps1` a second time while the demo runs: it refuses ("a demo started earlier is still running").
- [ ] `powershell -ExecutionPolicy Bypass -File scripts/demo-down.ps1` – it lists each process as stopped and each port as free.
- [ ] Look at `releases\v1.0.0\README.md` and check the checksums with the snippet in it (every line `ok`).

## B4 – plan and risk register

- [ ] Read `docs/capstone/plan.md`. The "challenge" between Revision 1 and Revision 2 was raised by Claude on your behalf (you delegated approvals). If you want a real challenge of your own, write it under the challenge heading and ask for a Revision 3.

## B5 – acceptance tests (red on purpose)

- [ ] Skim `docs/capstone/red-output.txt`: every failing test is mapped to an AC and fails because the feature is missing (404 route, missing migration, the F1 bug), not because of a typo.

## B6 – migration and backfill

Use a COPY of your database; never point a test at your real `demo.db`.

- [ ] Stop your own servers first (they hold the file open). Then: `Copy-Item demo.db demo-copy.db`.
- [ ] Migrate the copy with the new build (any command that opens it migrates it), for example: `.\storecraft.exe backfill-usage -db demo-copy.db` - it prints how many tenants it processed and how many values it rewrote in canonical form.
- [ ] Run it again: "0 values rewritten" and the same tenant count (it changes nothing the second time).
- [ ] Run the OLD binary on the migrated copy: `$env:STORECRAFT_ADMIN_TOKEN = "manual-admin"; .\releases\v1.0.0\storecraft.exe serve -addr :18095 -db demo-copy.db`, write and delete a key with a tenant token (curl.exe), stop it with Ctrl+C. It must work normally.
- [ ] Start the new build on the copy and check `GET /v1/usage` for that tenant matches the number of keys it lists.

## B7 – quota API (new storecraft.exe)

Your servers on :8090/:8091 still run the OLD binary. Stop them first (Ctrl+C in their windows), then use `scripts/demo-up.ps1` for a fresh demo with the new build.

- [ ] As admin: `curl.exe -X PUT -H "Authorization: Bearer <admin>" -H "If-Match: \"0\"" -d "{\"enabled\":true}" http://127.0.0.1:8090/v1/admin/flags/quota_enforcement` -> `{"enabled":true,"version":1}`.
- [ ] Set a small quota: `curl.exe -X PUT -H "Authorization: Bearer <admin>" -H "If-Match: \"0\"" -d "{\"max_keys\":2,\"max_bytes\":null}" http://127.0.0.1:8090/v1/admin/tenants/1/quota`.
- [ ] As tenant demo-1 write 3 keys: the third returns 403 `quota_exceeded` with `"used":2,"limit":2`. Delete one key, and the write works again.
- [ ] `curl.exe -H "Authorization: Bearer <demo-1 token>" http://127.0.0.1:8091/v1/usage` on the REPLICA shows the same numbers as the primary.
- [ ] Run `go test -race -count=20 -run Quota ./internal/...` yourself (about 6 minutes).

## B8 – quota console

With a fresh demo (`scripts/demo-up.ps1`) and two browser windows:

- [ ] Admin window: the "Quota enforcement" panel says off. Turn it on (a confirmation names the effect). Within 5 seconds the tenant window shows "Storage usage" without a reload.
- [ ] Tenant window: create a key in another window or with curl; the meter numbers change within 2 seconds.
- [ ] Admin: choose demo-1 under "Tenant quotas", set "Max keys" to the current number of keys, Save. Tenant: create a new key: the editor stays open, your text is kept, and the red message says the key limit with the numbers and what to do.
- [ ] Two admin windows on the same tenant quota: save in one, then save different values in the other: the conflict dialog shows "Your values" and "Latest values (version N)"; try Cancel (Esc), Reload and Overwrite.
- [ ] Use only the keyboard on the admin page (Tab, Enter, Esc): every control is reachable and focus is visible.
- [ ] Turn enforcement off: the meter and the quota editor disappear in both windows within 5 seconds.

## B9 – trace table and release candidate

- [ ] Open `docs/capstone/trace.md` and spot-check three rows: the test name exists at that line, and the code location is real.
- [ ] Check `releases\v2.0.0-rc.1` with the checksum snippet in its README (every line `ok`).

## B10 – flag and alert

- [ ] Read `docs/capstone/alert.md` (rule, threshold, why, runbook). If you have Prometheus somewhere, paste the rule into a rules file and run `promtool check rules` (action item AI-5).

## B11/B12 – game day (YOU must do the blind one)

- [ ] Follow `docs/capstone/gameday-prep.md`: choose and apply one fault without telling Claude, then give Claude only the symptom. The incident in `incident-timeline.md` and the postmortem were a real CI failure, used as a rehearsal; they are NOT the blind game day.

## B14 – release readiness and rollback drill

- [ ] Read `docs/capstone/drill-log.md`: every verify in the required legs says `OK: zero divergence`, and every smoke table passes.
- [ ] Run the drill yourself: `powershell -ExecutionPolicy Bypass -File scripts/rollback-drill.ps1` (about 1 minute; ports 18290/18291; it rewrites drill-log.md).
- [ ] Read `docs/capstone/release-readiness.md` and decide go/no-go row 7 (human peer review).

## B13 – review triage

- [ ] Read `docs/capstone/review-triage.md`. The reviewer was a fresh AI context, not a person: ask a human peer to review and approve (go/no-go row 7 in release-readiness.md).

## B15 – release v2.0.0 and your part of the wrap-up

- [ ] Check `releases\v2.0.0` with the checksum snippet in its README (7 lines `ok`, including RELEASE_NOTES.md).
- [ ] Fill in YOUR self-ratings (start / middle / end) and your own words in `docs/retrospective-week2.md`.
- [ ] Read `docs/capstone/rubric-selfscore.md`, open every evidence link, and adjust any score you disagree with.
- [ ] Rehearse `docs/demo-script.md` once with `scripts/demo-up.ps1` (stop your :8090/:8091 servers first).
- [ ] Optional cleanup after you stop your old server: delete `storecraft.exe~` in the project root.
