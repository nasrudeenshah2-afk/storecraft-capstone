# Game day (B11): what YOU do, and what was done without you

## Why this needs you

Step B11 only works if the person investigating does **not** know the cause. You were asked to complete everything autonomously, but Claude cannot pick a fault and then pretend not to know it. So:

- **Done without you:** a *rehearsal* game day (fault chosen by Claude, so NOT blind), clearly labelled as such in `incident-timeline.md` and the postmortem. It exercises the tooling: mitigation, logs with timestamps, metrics, `verify`, and a regression test.
- **Still to do by you (about 30 minutes):** the real, blind game day below. Then give Claude only the symptom, and it redoes B11 and B12 for real.

## The real game day (blind)

1. Start a fresh demo with the current build: `powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1`. Turn quota enforcement on in the console (admin page).
2. Start some load in a second PowerShell window. This writes and deletes keys as tenant demo-1 for about a minute (synthetic data); the token comes from `demo-tokens.txt`:
   ```powershell
   $t = (Select-String -Path demo-tokens.txt -Pattern '^demo-1: (.+)$').Matches[0].Groups[1].Value
   1..300 | ForEach-Object {
     curl.exe -s -o NUL -X PUT -H "Authorization: Bearer $t" -d "{\"value\":$_}" "http://127.0.0.1:8090/v1/keys/load-$($_ % 40)"
     if ($_ % 3 -eq 0) { curl.exe -s -o NUL -X DELETE -H "Authorization: Bearer $t" "http://127.0.0.1:8090/v1/keys/load-$($_ % 40)" }
   }
   ```
3. While it runs, apply **one** fault of your choice and do not write it down where Claude can read it:
   - run `.\storecraft.exe backfill-usage -db demo.db` twice during the load
   - stop the replica during writes (`scripts/demo-down.ps1` stops everything, or close the replica's window), then start it again
   - change a stored value or timestamp in the replica's database file (for example with the `sqlite3` tool, if you have it)
   - stop the primary and restart it while the console is open
4. Look at the console, `/metrics` and `.\storecraft.exe verify ...`, and describe **only what you see**.
5. Paste into Claude Code: `STEP B11. We have an incident. Symptom only: <what you saw>` (the prompt in `docs/prompts.md`).
