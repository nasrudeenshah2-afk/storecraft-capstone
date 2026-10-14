# Rollback drill log

Written by `scripts/rollback-drill.ps1` on 2026-10-14 00:24:32. Every line below is real output from this run; data is synthetic, in `C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426`, on ports 18290 (primary) and 18291 (replica).

| build | path | SHA-256 (first 16) |
|---|---|---|
| current (built from the source tree now) | `C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426\storecraft-current.exe` | 83692c27cfe37e1e |
| rollback target | `C:\Users\NasrudeenShahS\Documents\code\storecraft-capstone\releases\v2.0.0-rc.1\storecraft.exe` | 3d7c7e0296efa1a9 |
| previous release (extra leg) | `C:\Users\NasrudeenShahS\Documents\code\storecraft-capstone\releases\v1.0.0\storecraft.exe` | e4ccb4c033131244 |

## Leg 1: current build, fresh data
- 2026-10-14 00:24:46 started with `demo-up.ps1 -Binary C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426\storecraft-current.exe`: primary PID 14736, replica PID 32240
- 2026-10-14 00:24:50 wrote 60 keys (values with extra whitespace and < > &) across 3 tenants, deleted 6
- 2026-10-14 00:24:50 flag on: 200 {"enabled":true,"version":1}
- 2026-10-14 00:24:50 demo-2 quota 18 keys: 200 {"max_keys":18,"max_bytes":null,"version":1}
- 2026-10-14 00:24:50 a 19th key for demo-2: 403 {"error":{"code":"quota_exceeded","message":"This write would add a key, but 18 of 18 keys are used.","details":{"dimension":"keys","limit":18,"requested":1,"used":18}}}
- 2026-10-14 00:24:50 **verify (current, after writes)**: exit 0 (expected 0) -> as expected
```text
OK: zero divergence (primary and replica both at seq 70: 3 tenants, 54 keys, 1920 value bytes)
```
- 2026-10-14 00:24:50 **smoke (current)**: PASS

  | check | status | result |
  |---|---|---|
  | sign in (GET /v1/keys) | 200 | 18 keys |
  | read drill/k001 | 200 | same value as written in leg 1 |
  | write drill/smoke-current | 201 | {"key":"drill/smoke-current","version":1} |
  | read usage | 200 | {"tenant_id":1,"keys":{"used":19,"limit":1000},"bytes":{"used":664,"limit":1048576},"seq":71} |

- 2026-10-14 00:24:53 stopped with `demo-down.ps1` (exit 0): replica  PID 32240: stopped; primary  PID 14736: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.

## Leg 2: roll back to releases/v2.0.0-rc.1 on the same data
- 2026-10-14 00:24:56 started with `demo-up.ps1 -Binary C:\Users\NasrudeenShahS\Documents\code\storecraft-capstone\releases\v2.0.0-rc.1\storecraft.exe -KeepData`: primary PID 31976, replica PID 24040
- 2026-10-14 00:24:56 **verify (after rollback)**: exit 0 (expected 0) -> as expected
```text
OK: zero divergence (primary and replica both at seq 71: 3 tenants, 55 keys, 1944 value bytes)
```
- 2026-10-14 00:24:57 **smoke (rollback rc1)**: PASS

  | check | status | result |
  |---|---|---|
  | sign in (GET /v1/keys) | 200 | 19 keys |
  | read drill/k001 | 200 | same value as written in leg 1 |
  | write drill/smoke-rollbackrc1 | 201 | {"key":"drill/smoke-rollbackrc1","version":1} |
  | read usage | 200 | {"tenant_id":1,"keys":{"used":20,"limit":1000},"bytes":{"used":693,"limit":1048576},"seq":72} |

- 2026-10-14 00:24:59 stopped with `demo-down.ps1` (exit 0): replica  PID 24040: stopped; primary  PID 31976: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.

## Leg 3: roll forward to the current build on the same data
- 2026-10-14 00:25:03 started with `demo-up.ps1 -Binary C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426\storecraft-current.exe -KeepData`: primary PID 22660, replica PID 29864
- 2026-10-14 00:25:03 **verify (after roll-forward)**: exit 0 (expected 0) -> as expected
```text
OK: zero divergence (primary and replica both at seq 72: 3 tenants, 56 keys, 1973 value bytes)
```
- 2026-10-14 00:25:03 **smoke (roll forward)**: PASS

  | check | status | result |
  |---|---|---|
  | sign in (GET /v1/keys) | 200 | 20 keys |
  | read drill/k001 | 200 | same value as written in leg 1 |
  | write drill/smoke-rollforward | 201 | {"key":"drill/smoke-rollforward","version":1} |
  | read usage | 200 | {"tenant_id":1,"keys":{"used":21,"limit":1000},"bytes":{"used":722,"limit":1048576},"seq":73} |

- 2026-10-14 00:25:06 stopped with `demo-down.ps1` (exit 0): replica  PID 29864: stopped; primary  PID 22660: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.

## Extra leg: roll back to releases/v1.0.0 (the previous release), then forward again
Expected (documented in release-readiness.md): v1.0.0 runs on the migrated data but has no usage API; its replica re-escapes < > & (finding F1), so verify reports those keys; after rolling forward, usage counters of values written by v1.0.0 differ between the nodes until backfill-usage has run on both.
- 2026-10-14 00:25:09 started with `demo-up.ps1 -Binary C:\Users\NasrudeenShahS\Documents\code\storecraft-capstone\releases\v1.0.0\storecraft.exe -KeepData`: primary PID 10588, replica PID 27692
- 2026-10-14 00:25:09 v1.0.0 accepted a write with whitespace and < >: 201 {"key":"drill/v1-write","version":1}
- 2026-10-14 00:25:09 **verify (on v1.0.0 (F1 expected))**: exit 1 (expected 1) -> as expected
```text
DIVERGED: 1 difference(s) with both nodes at seq 74
  tenant demo-1 (id 1) key "drill/v1-write": primary version 1 sha256 e80199e88dee234cfc730720897d6d4f433cec3a969d3db421ffc321840ef1ff / replica version 1 sha256 68f37f0d20a06f4cd859105bcc78b8ca71195cf1bb931f1381f7ccd85b0b849a
```
- 2026-10-14 00:25:10 **smoke (v1.0.0)**: PASS

  | check | status | result |
  |---|---|---|
  | sign in (GET /v1/keys) | 200 | 22 keys |
  | read drill/k001 | 200 | same value as written in leg 1 (sent with HTML-escaped JSON: old encoder) |
  | write drill/smoke-v100 | 201 | {"key":"drill/smoke-v100","version":1} |
  | read usage | 404 | expected 404: v1.0.0 has no usage API |

- 2026-10-14 00:25:12 stopped with `demo-down.ps1` (exit 0): replica  PID 27692: stopped; primary  PID 10588: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.
- 2026-10-14 00:25:15 started with `demo-up.ps1 -Binary C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426\storecraft-current.exe -KeepData`: primary PID 9224, replica PID 35084
- 2026-10-14 00:25:15 **verify (current again, before backfill (usage_mismatch expected))**: exit 1 (expected 1) -> as expected
```text
DIVERGED: 1 difference(s) with both nodes at seq 75
  tenant demo-1 (id 1): usage_mismatch (primary 23 keys / 776 bytes, replica 23 keys / 782 bytes)
```
- 2026-10-14 00:25:18 stopped with `demo-down.ps1` (exit 0): replica  PID 35084: stopped; primary  PID 9224: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.
- 2026-10-14 00:25:18 `backfill-usage -db demo.db` (both nodes stopped): exit 0
```text
backfill-usage: 3 tenants processed, 0 already done by an earlier run, 1 values rewritten in canonical form
complete: every tenant's usage equals its data
```
- 2026-10-14 00:25:18 `backfill-usage -db demo-replica.db` (both nodes stopped): exit 0
```text
backfill-usage: 3 tenants processed, 0 already done by an earlier run, 1 values rewritten in canonical form
complete: every tenant's usage equals its data
```
- 2026-10-14 00:25:22 started with `demo-up.ps1 -Binary C:\Users\NASRUD~1\AppData\Local\Temp\storecraft-drill-20261014-002426\storecraft-current.exe -KeepData`: primary PID 15508, replica PID 23488
- 2026-10-14 00:25:22 **verify (current, after backfill on both nodes)**: exit 0 (expected 0) -> as expected
```text
OK: zero divergence (primary and replica both at seq 75: 3 tenants, 59 keys, 2052 value bytes)
```
- 2026-10-14 00:25:22 **smoke (final)**: PASS

  | check | status | result |
  |---|---|---|
  | sign in (GET /v1/keys) | 200 | 23 keys |
  | read drill/k001 | 200 | same value as written in leg 1 |
  | write drill/smoke-final | 201 | {"key":"drill/smoke-final","version":1} |
  | read usage | 200 | {"tenant_id":1,"keys":{"used":24,"limit":1000},"bytes":{"used":794,"limit":1048576},"seq":76} |

- 2026-10-14 00:25:25 stopped with `demo-down.ps1` (exit 0): replica  PID 23488: stopped; primary  PID 15508: stopped; port 18290 (primary) is free; port 18291 (replica) is free; demo-down: demo stopped; all its ports are free.

## Result
**PASS**: every verify gave the expected verdict (zero divergence in all required legs) and every smoke check passed.
