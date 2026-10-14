# STORE-210 spec: per-tenant storage quotas, usage metering and console

| | |
|---|---|
| Status | **Approved** by the reviewer (relayed by the user, 2026-10-13). Changes from here on need a new approval. |
| Ticket | STORE-210 |
| Date | 2026-10-13 |
| Inputs | `docs/capstone/intake.md` (B1); the B2 proposal approved by the product owner on 2026-10-13; the product-owner table in `docs/prompts.md` |

Acceptance criteria (ACs) are numbered `AC-API-n` and `AC-UI-n` so that tests, code and the trace table (B9) can refer to them. Each AC states what a test must observe to **pass**; anything else is a **fail**.

---

## 1. Problem

storecraft tenants can store an unlimited number of keys and bytes. One tenant can therefore fill the shared disk and slow everyone down, and nobody can see how much space a tenant uses.

STORE-210 gives every tenant:
- a maximum key count and a maximum total value size, enforced when writing
- a way to read its own usage
- an admin way to change limits safely

The console shows usage live, lets admins edit quotas without overwriting each other, and explains clearly what happened when a save is rejected for quota. Usage must stay correct under concurrent writes, across restarts and on replicas, and must be visible in metrics.

## 2. Definitions

| Term | Meaning |
|---|---|
| **Canonical value** | A value's JSON in compact form, with no insignificant whitespace, no HTML escaping (`<`, `>`, `&` stay as they are), and object keys in their original order. |
| **Value bytes** | The UTF-8 length of the canonical value. Key names never count. |
| **Usage** | Per tenant: `keys` = number of stored keys; `bytes` = sum of value bytes. |
| **Quota** | Per tenant: `max_keys` and `max_bytes`, each an integer ≥ 0 or `null` (= unlimited), plus a `version` used for CAS. |
| **Default quota** | `max_keys = 1000`, `max_bytes = 1048576` (1 MiB). Applies to every tenant that has no quota record (version 0). |
| **Growing write** | A PUT that adds a key, or that makes the tenant's total bytes larger. |
| **Enforcement** | Rejecting growing writes that would exceed a quota. Controlled by the flag `quota_enforcement`. |

## 3. Decisions (approved proposal)

1. FACT: values are stored canonically from STORE-210 on. Replication carries the bytes unchanged, so primary and replica hold identical bytes. This also fixes intake finding F1, the false `verify` divergence for `<`, `>` and `&`.
2. **Schema** (migration 6, additive only):
   - `tenant_quotas(tenant_id, max_keys, max_bytes, version, updated_at)`
   - `tenant_usage(tenant_id, key_count, byte_count)`
   - triggers on `kv` that keep `tenant_usage` correct in the same transaction as every insert, update or delete
   - `flags(name, enabled, version, updated_at)`
   - a new nullable column `changes.subject`
3. **Replication:**
   - Quota changes travel in the change log as `kind='tenant'`, `subject='quota'` rows.
   - Flag state travels as an extra field of the admin change feed.
   - Usage is never shipped: each node's triggers compute it from its own `kv` rows.
4. **Over quota**: HTTP **403** with code **`quota_exceeded`** and a `details` object (see 5.3).
5. **Missing `If-Match`** on quota or flag edits: HTTP **428**, code **`precondition_required`** (new code).
6. **Flag default**: `quota_enforcement` is **off** until an admin turns it on (dark launch).

## 4. Contract summary

`api/openapi.yaml` is updated with these in step B3. It is the authority for exact shapes.

| Method and path | Auth | Success |
|---|---|---|
| `GET /v1/usage` | tenant token | `200 {"tenant_id","keys":{"used","limit"},"bytes":{"used","limit"},"seq"}` (`limit: null` = unlimited) |
| `GET /v1/admin/tenants` | admin token | `200 {"tenants":[{"id","name","quota":{"max_keys","max_bytes","version"},"usage":{"keys","bytes"}}]}` sorted by id |
| `GET /v1/admin/tenants/{id}` | admin | `200` one tenant, same fields |
| `GET /v1/admin/tenants/{id}/quota` | admin | `200 {"max_keys","max_bytes","version"}`, header `ETag: "<version>"` |
| `PUT /v1/admin/tenants/{id}/quota` | admin, `If-Match` required | `200` new quota and version, new `ETag` |
| `GET /v1/flags` | tenant or admin | `200 {"quota_enforcement":{"enabled","version"}}` |
| `PUT /v1/admin/flags/quota_enforcement` | admin, `If-Match` required | `200 {"enabled","version"}`, new `ETag` |

## 5. API behaviour

### 5.1 Order of checks on `PUT /v1/keys/{key}`
1. Replica: 409 `read_only_replica`.
2. Authentication: 401 / 403 `forbidden`.
3. Request body and headers: 400 `invalid_request`.
4. Idempotent replay: the stored response is returned. A replay is **never counted and never rejected**.
5. Version preconditions: 412 `precondition_failed` / 404.
6. Quota: 403 `quota_exceeded`, only when enforcement is on and the write is growing.

DELETE is never rejected for quota.

### 5.2 What counts
- A new key adds 1 key and its value bytes.
- An overwrite counts only the difference (new bytes − old bytes). A shrinking or equal overwrite is always allowed, even when the tenant is over quota.
- A delete frees its key and bytes.
- If a write would break both limits, `details.dimension` is `keys`.

### 5.3 Error shapes
All errors use the standard envelope. `quota_exceeded` adds `details`:
```json
{"error": {"code": "quota_exceeded",
  "message": "This write needs 2048 more bytes, but 1047000 of 1048576 bytes are used.",
  "details": {"dimension": "bytes", "used": 1047000, "limit": 1048576, "requested": 2048}}}
```
- `dimension` is `keys` or `bytes`.
- `requested` is the number of keys (1) or bytes the write would add.
- The message always contains the used and limit numbers.

Quota and flag edits: 400 `invalid_request` (bad body, a negative or non-integer limit, unknown fields), 401 / 403, 404 `not_found` (unknown tenant), 409 `read_only_replica`, 412 `precondition_failed` (stale version), 428 `precondition_required` (no `If-Match`).

## 6. Acceptance criteria: API

**AC-API-1 Key limit.**
- With enforcement on and a tenant at `max_keys` keys, a PUT that creates a new key returns 403 `quota_exceeded` with `details.dimension = "keys"`, `used = max_keys`, `limit = max_keys` and `requested = 1`.
- **Pass when:** that key then returns 404, no change-log row was added, and `GET /v1/usage` is unchanged. An overwrite of an existing key in the same state succeeds.

**AC-API-2 Byte limit and overwrite delta.**
- With enforcement on, a growing write whose new total would exceed `max_bytes` returns 403 with `dimension = "bytes"` and `requested` = the exact byte difference, and nothing is written.
- **Pass when all of these hold:**
  - an overwrite that keeps the total within the limit succeeds, even if the new value alone is larger than the free space
  - a shrinking overwrite succeeds while the tenant is over quota
  - a delete succeeds and lowers `bytes.used` by the deleted value's bytes

**AC-API-3 Correct under concurrency.**
- Run 100 concurrent writers against a tenant 10 keys below `max_keys` (and, in a second run, near `max_bytes`).
- **Pass when, after the run:** exactly as many creates succeeded as there was headroom (10 for keys); `keys.used ≤ max_keys` and `bytes.used ≤ max_bytes`; and `GET /v1/usage` equals a direct `COUNT(*)` and the sum of value bytes over the tenant's keys.
- This holds in every run of `go test -race -count=20 -run Quota ./internal/...`.

**AC-API-4 Reading usage.**
- `GET /v1/usage` with a tenant token returns that tenant's own `keys`/`bytes` (`used` and `limit`) and the `seq` it was read at, and `used` equals the real data. There is no way to name another tenant.
- `GET /v1/admin/tenants/{id}` with the admin token returns any tenant's usage and quota.
- **Pass when:** a tenant token on any `/v1/admin/...` route gets 403 `forbidden`; no token or a wrong token gets 401.

**AC-API-5 Quota edit with CAS.**
- `GET .../quota` returns the version in `ETag`.
- **Pass when all of these hold:**
  - `PUT` with the matching `If-Match` returns 200 and version + 1
  - with a stale version it returns 412 and the quota is unchanged
  - without `If-Match` it returns 428
  - with a negative, non-integer or missing limit field it returns 400
  - for an unknown tenant it returns 404
- `null` means unlimited.
- Lowering a quota below current usage is allowed. Afterwards growing writes get 403, while deletes and shrinking overwrites succeed.

**AC-API-6 Defaults.**
- A tenant created by `storecraft seed`, by `CreateTenant`, or by the v1.0.0 binary has no quota record and reports `max_keys = 1000`, `max_bytes = 1048576` and quota version 0.
- **Pass when:** its first quota edit uses `If-Match: "0"`.

**AC-API-7 Idempotency never double counts.**
- **Pass when all of these hold:**
  - replaying an accepted write with the same `Idempotency-Key` returns the stored response and leaves usage unchanged
  - a write rejected with 403 stores no idempotency record
  - after the admin raises the quota, the same request with the same key succeeds once and is counted once

**AC-API-8 Survives restart.** **Pass when** usage, quotas and the flag read back identically after the server is stopped and started on the same database.

**AC-API-9 Canonical bytes (fixes F1).**
- A value sent with extra whitespace or containing `<`, `>` or `&` is stored in canonical form. `bytes.used` counts the canonical UTF-8 length; a non-ASCII value counts bytes, not characters.
- **Pass when:** after replication, the stored bytes on primary and replica are identical and `storecraft verify` reports zero divergence.

**AC-API-10 Replicas.**
- A replica serves `GET /v1/usage`, `GET /v1/flags`, `GET /v1/admin/tenants` and the per-tenant reads from its own replicated state. After it has applied the primary's seq, the results equal the primary's.
- **Pass when:** `PUT` on quota or flag routes of a replica returns 409 `read_only_replica`, and after a load run on the primary, `storecraft verify` (which now also compares usage and quotas) reports zero divergence.

**AC-API-11 Feature flag.**
- `GET /v1/flags` works with a tenant or admin token.
- With the flag **off**, writes beyond a quota succeed, while usage is still counted and reported.
- `PUT /v1/admin/flags/quota_enforcement` with `If-Match` changes it (412 when stale, 428 when missing). The very next write request on the primary obeys the new value with no restart.
- **Pass when:** toggling it several times during a concurrent load produces zero failed requests other than expected `quota_exceeded` responses.

**AC-API-12 Old binary on the new schema.**
- The `releases/v1.0.0` binary starts against a migrated database and serves reads and writes.
- **Pass when:** after it writes and deletes keys, the database's usage still equals the real data (the triggers keep it correct), and the new binary afterwards reports the right usage without running a backfill.

**AC-API-13 Backfill.**
- **Pass when all of these hold:**
  - `storecraft backfill-usage` sets every tenant's usage to a direct `COUNT`/`SUM` over `kv` and stores old values in canonical form
  - running it twice changes nothing
  - stopping it midway and running it again gives the same final result

## 7. Acceptance criteria: console (UI)

Every UI AC must also pass an axe check with **zero violations** on the screen in the state described.

**AC-UI-1 Usage meter: states.**
- A tenant sees a usage meter for keys and for bytes on the Keys page.
- Each shows text such as "12 of 1,000 keys" and "3.4 KB of 1 MB", plus a meter element with `role="meter"`, `aria-valuenow`, `aria-valuemax` and an accessible name.
- It has distinct states:
  - **loading** (text "Loading usage…")
  - **error** (a message and a "Try again" button)
  - **unlimited** (the text "unlimited", no meter value)
  - **near limit**, at 80% or more (the word "Almost full" in text, not colour alone)
  - **over quota** (the words "Over quota" in text)
- **Pass when:** each state can be reached with the stubbed API and shows exactly these texts and roles.

**AC-UI-2 Usage meter: live.** **Pass when**, with the console open, the meter's numbers change within 2 seconds after another client creates, overwrites or deletes a key of the same tenant, without a page reload.

**AC-UI-3 Over-quota at save.**
- When a save returns 403 `quota_exceeded`, the editor stays open with the user's text unchanged. It shows a message that is announced (`role="alert"`) and contains: which limit (keys or bytes), the used and limit numbers from `details`, and what to do ("Delete keys you no longer need, or ask an admin to raise the quota").
- The message is chosen by the error **code**.
- **Pass when:** changing the server's message text does not change what the console shows, and a 403 `forbidden` shows a different message.

**AC-UI-4 Admin quota editor.**
- An admin chooses a tenant from a list (from `GET /v1/admin/tenants`) and sees its current quota and usage.
- They edit "Max keys" and "Max bytes"; an empty field or an "Unlimited" checkbox means `null`.
- Invalid input (negative, decimal, text) shows a field error and sends nothing.
- Save sends `If-Match` with the version shown.
- **Pass when:** a successful save shows the new values and version.

**AC-UI-5 Quota editor conflict.**
- When the quota save returns 412, a conflict dialog shows "Your values" and "Latest values (version N)", with **Overwrite** (re-send with the latest version), **Reload** (load the latest into the editor) and **Cancel** (keep my input).
- **Pass when:** each choice behaves as named, and the dialog is keyboard-operable (Tab stays inside, Esc cancels, focus returns to the Save button).

**AC-UI-6 Flag hides the feature live.**
- **Pass when:** with `quota_enforcement` off, the usage meter, the quota editor and quota messages are not shown, and when an admin turns the flag on or off, every open console shows or hides the quota feature within 5 seconds without a page reload.

**AC-UI-7 Admin flag toggle.**
- An admin sees the current state of `quota_enforcement` and can switch it, after a confirmation step that names the effect ("Writes over quota will be rejected").
- **Pass when:** the toggle sends `If-Match`, and a 412 shows a message asking to reload the current state.

**AC-UI-8 Accessibility and keyboard.**
- **Pass when all of these hold:**
  - the meter, the over-quota message, the quota editor, its conflict dialog and the flag toggle are reachable and operable with the keyboard alone
  - every input has a visible label
  - status changes are announced (`aria-live`)
  - axe reports zero violations on the Keys page (normal, near-limit and over-quota states) and on the admin page (editor open and conflict dialog open)

## 8. Behaviour on replicas

- FACT, existing behaviour: replicas never accept writes (409 `read_only_replica`); that now also covers quota and flag edits.
- Replicas answer usage, quota, tenant and flag reads from replicated state (AC-API-10). Their usage comes from their own triggers over replicated `kv` rows; quotas come from replicated `subject='quota'` change rows; the flag comes from the admin feed.
- Enforcement happens only on the primary, the only node that accepts writes.
- INFERENCE: a v1.0.0 replica skips quota change rows (it sees an existing tenant and does nothing) and ignores the flags field. It keeps replicating keys, but serves no quota data. Upgrade order: replicas first, then the primary.

## 9. Observability

**Server metrics** (all on `/metrics`):

| Metric | Type | Meaning |
|---|---|---|
| `storecraft_tenant_usage_keys{tenant_id}` | gauge | keys used, read from `tenant_usage` at scrape time |
| `storecraft_tenant_usage_bytes{tenant_id}` | gauge | bytes used, same |
| `storecraft_quota_rejections_total{dimension}` | counter | 403 `quota_exceeded` responses, by `keys` / `bytes` |
| `storecraft_quota_enforcement_enabled` | gauge | 1 when the flag is on, else 0 |

The two usage gauges are the only metrics allowed a `tenant_id` label; the cardinality is bounded by the number of tenants. The "no tenant labels" rule and its test are changed to allow exactly this exception.

**Client events.** The console records named events for tests and future telemetry. They are dispatched in the browser and kept in memory; they are not sent anywhere (see non-goals). No event ever contains a token or a key's value.

| Event | When | Properties |
|---|---|---|
| `quota_rejected_shown` | the over-quota message is shown | `dimension` |
| `quota_editor_saved` | a quota save succeeds | `tenant_id`, `version` |
| `quota_editor_conflict` | a quota save hits 412 | `tenant_id` |
| `quota_flag_toggled` | the admin toggles the flag | `enabled` |
| `usage_near_limit_shown` | the meter enters the near-limit state | `dimension` |

**Alert rule** (written out in `docs/capstone/alert.md` in step B10):
- **QuotaRejectionsHigh**: `sum(rate(storecraft_quota_rejections_total[5m])) > 1` for 10 minutes.
- Meaning: tenants are hitting quotas continuously, so either a quota is too low or a client is misbehaving.

**Logs.** The server logs every quota and flag change with tenant id, old and new values, and versions, never tokens.

## 10. Feature-flag behaviour (`quota_enforcement`)

- **Storage:** stored on the primary (`flags` table), replicated through the admin feed, and read on every request from one place. No restart is needed.
- **Default:** off.
- **Off:** no `quota_exceeded` rejections; usage is still counted and served; the console hides the meter, the quota editor and quota messages; the admin page still shows the flag toggle.
- **On:** enforcement as in section 5; the console shows the quota feature.
- **Toggling:** admin only, CAS with `If-Match` (AC-API-11, AC-UI-7). The console picks up changes within 5 seconds (AC-UI-6).

## 11. Non-goals

- Limits per key, or a value-size limit other than the existing 1 MiB request body.
- Rate limiting, billing, or usage history and graphs.
- Tenants changing their own quota, or self-service tenant creation.
- Notifications (email or chat) when a tenant nears its quota.
- Enforcing quotas on replicas (they accept no writes).
- Sending client events to a server or third party.
- Quotas for the admin token; counting key names as usage.

## 12. Open items for the reviewer

- INFERENCE: the "replicas first, then primary" upgrade order (section 8) should be confirmed in the release plan (B14).
- UNKNOWN: a realistic number of tenants per deployment, which bounds the metrics cardinality in section 9.
