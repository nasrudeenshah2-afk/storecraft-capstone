# Demo script: per-tenant quotas in the storecraft console (10 minutes)

**For:** a presenter showing STORE-210 to reviewers. **Needs:** two browser windows side by side, one PowerShell window.

## Before the audience arrives (2 minutes, not part of the 10)

1. Stop any storecraft you have running (demo-up refuses busy ports).
2. `powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1`. It prints the console URL (http://127.0.0.1:5173) and writes the tokens to `demo-tokens.txt`.
3. **Window A (left):** sign in as **Admin** with the `admin:` token. **Window B (right):** sign in as **Tenant** with the `demo-1:` token.
4. In window B, create 3 keys (for example `app/theme` = `"dark"`, `app/lang` = `"en"`, `app/limit` = `10`).

## 0:00 – 1:00 The problem (talk only)

Say: "Before this release any tenant could store unlimited data and fill the shared disk, and nobody could see how much space a tenant used. STORE-210 adds per-tenant limits, live usage, and safe admin controls. It shipped dark: nothing changes until an admin switches it on."

Point at window B: **no usage meter is visible**. The feature is off.

## 1:00 – 2:30 Turn it on (AC-UI-7, AC-UI-6)

1. **Window A, "Quota enforcement" panel:** point at "Quota enforcement is off". Click **Turn on**.
2. Point at the confirmation: **"Writes over quota will be rejected."** Say: "It names the effect before you commit." Click **Turn on**.
3. **Window B:** within about 3 seconds, **"Storage usage"** appears, with no reload. Say: "Every open console follows the flag within 5 seconds."
4. Point at the numbers: **"3 of 1,000 keys"** and the bytes line (for example **"18 bytes of 1 MB"**). Say: "This is the default quota: 1,000 keys and 1 MB per tenant."

## 2:30 – 4:00 Live usage (AC-UI-1, AC-UI-2)

1. **Window B:** create a key `app/notes` with a longer value, for example `{"text": "hello world, this is a longer value"}`.
2. Point at the meter: keys **3 → 4**, and the bytes grow. Say: "The meter is driven by the same live change feed as the key list."
3. Optional: in PowerShell, write a key with curl as the same tenant. The meter moves within 2 seconds without anyone clicking.
4. Say: "Every number is also text, and the meter has a screen-reader name; nothing is said with colour alone."

## 4:00 – 6:00 Hitting the limit (AC-UI-3, AC-API-1)

1. **Window A, "Tenant quotas":** choose **demo-1**. Point at "Usage: 4 keys …" and **"Version 0 (default quota, never edited)"**.
2. Set **Max keys** to `4` and click **Save quota**. Point at **"Quota saved (version 1)."**
3. **Window B:** the meter shows **"4 of 4 keys – Almost full"** within 10 seconds. Click **New key**, type `app/extra` = `1`, and click **Create**.
4. Point at the red message: **"Nothing was saved: this would go over your key limit (4 of 4 keys are used). Delete keys you no longer need, or ask an admin to raise the quota."**
5. Say: "The editor stays open and the text is kept. The numbers come from the server's error details, and the message is chosen by the error code, never by parsing text."
6. Click **Cancel**, delete `app/notes`, then create `app/extra` again: it works. Say: "Deletes always work, even when over quota."

## 6:00 – 8:00 Two admins at once (AC-UI-5, AC-API-5)

1. Open a **third window** as Admin (or use curl). Both admin views show demo-1 at **Version 1**.
2. Third window: set Max keys `10` and save (Version 2).
3. Window A (still showing version 1): set Max keys `20` and click **Save quota**.
4. Point at the **conflict dialog**: "Your values" (20) and **"Latest values (version 2)"** (10). Say: "Every quota save carries the version it was based on (If-Match). Nobody silently overwrites a colleague."
5. Press **Esc**. Focus returns to Save and the 20 is kept. Click **Save quota** again, then **Overwrite with my values**. Version 3.

## 8:00 – 9:00 Safe to switch off (AC-API-11, AC-UI-6)

1. **Window A:** **Turn off**. The confirmation says "Writes over quota will be accepted again".
2. **Window B:** the meter disappears within about 3 seconds. Create keys freely.
3. Say: "This is the incident lever: one click, takes effect on the next request, no restart, no data change. Usage keeps being counted while it is off."

## 9:00 – 10:00 Behind the scenes (numbers to quote)

- "Correct under concurrency: 100 writers at the same moment against 10 free slots gives exactly 10 successes, in every one of 20 race-detector runs." (`go test -race -count=20 -run Quota ./internal/...`)
- "The primary and its replica store byte-identical values now. `storecraft verify` compares usage and quotas too." (Optional: run `.\storecraft.exe verify -primary=http://127.0.0.1:8090 -replica=http://127.0.0.1:8091` with the admin token set: **OK: zero divergence**.)
- "Rollback is drilled: the release candidate and back on the same data, zero divergence." (`docs/capstone/drill-log.md`)
- Metrics, if asked: `storecraft_quota_rejections_total`, the per-tenant usage gauges, and the alert `QuotaRejectionsHigh`.

## After the demo

`powershell -ExecutionPolicy Bypass -File scripts/demo-down.ps1`
