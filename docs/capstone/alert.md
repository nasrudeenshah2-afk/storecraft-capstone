# Alert rule: QuotaRejectionsHigh

Step B10, 2026-10-13. Source of the requirement: `spec.md` section 9.

## The rule (Prometheus format)

```yaml
groups:
  - name: storecraft-quota
    rules:
      - alert: QuotaRejectionsHigh
        expr: sum(rate(storecraft_quota_rejections_total[5m])) > 1
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Writes are being rejected for quota (more than 1 per second for 10 minutes)"
          runbook: "docs/capstone/alert.md"
```

`internal/api/alert_test.go` reads the expression, threshold, window and `for` duration **from this file**, so the test and this document cannot drift apart.

## Condition, threshold and why

| Part | Value | Why |
|---|---|---|
| Signal | `storecraft_quota_rejections_total` (counter, labels `dimension="keys"\|"bytes"`) | Increments on every 403 `quota_exceeded` and on nothing else (`internal/api/writes.go` `apply`). It is a direct count of users being refused. |
| Aggregation | `sum(...)` over both dimensions | Whether keys or bytes ran out, the effect for the user is the same (a refused save). The per-dimension label stays available for diagnosis. |
| Rate window | `[5m]` | Smooths a single burst (one client retrying a few times) while still reacting within minutes. |
| Threshold | `> 1` per second | One tenant at its limit with a correctly behaving client produces a handful of rejections, then stops (the console explains the limit; nothing retries automatically). More than one per second, all the time, means a client is retrying in a loop or a quota is far too low for real use. |
| For | `10m` | A short spike, such as a migration or a batch job hitting a freshly lowered quota, should not page anyone. Ten minutes of sustained rejection is worth a look. |
| Severity | warning | Rejections are the feature working as designed. The alert says "look at this", not "the service is down". |

## What to do when it fires (runbook)

1. **Which dimension?** Look at `sum by (dimension) (rate(storecraft_quota_rejections_total[5m]))`.
2. **Which tenant?** Look at the usage gauges (`storecraft_tenant_usage_keys` / `_bytes`, labelled by `tenant_id`). Tenants at or above their quota show in the console admin page ("Tenant quotas"). The quota is in `GET /v1/admin/tenants`.
3. **Decide:**
   - A client retries in a loop → contact the tenant. The quota is doing its job.
   - The quota is too low for legitimate use → raise it in the console (CAS-protected), and the rejections stop at once.
   - Rejections are everywhere after a release → mitigate first by turning `quota_enforcement` **off** (admin page, or `PUT /v1/admin/flags/quota_enforcement`). This takes effect on the next request, with no restart and no data change. Then investigate.
4. Record what happened in `docs/incidents/`.

## How the alert is tested

`TestAlertRuleFiresInForcedScenario` (in `internal/api/alert_test.go`):
- forces 1.5 real `quota_exceeded` responses per simulated second on a real server
- scrapes `/metrics` after each simulated minute
- evaluates the rule above (`rate` = counter increase ÷ window, summed over dimensions, held for the `for` duration)

It proves that the alert **fires** at exactly minute 15: the first full 5-minute window plus 10 minutes. On given counter readings it also checks that the alert does **not** fire:
- at exactly the threshold
- for a single burst
- for 9 minutes above the threshold

`TestQuotaRejectionCounterCountsOnlyQuotaRejections` proves the counter counts exactly the 403 `quota_exceeded` responses: not a 412, 404, 401 or 400.

**Limitation:** INFERENCE. No Prometheus server is available in this environment, so the rule is evaluated by a small evaluator in the test, not by Prometheus itself. Prometheus' `rate()` also extrapolates at window edges, which this evaluator does not. With a steady load the two agree; near the threshold Prometheus may fire slightly earlier or later. Validating the rule with `promtool test rules` is listed as an action item for a deployment that has Prometheus.
