# API contract (schema version 1)

## Request

```http
GET /v1/quota HTTP/1.1
Authorization: Bearer <existing CPA client key>
```

`X-API-Key` is an alternative. Supplying both credential headers, duplicate headers, or query parameters is rejected. Keys in URLs are not accepted. Only GET is supported. This API is custom; OpenAI-compatible clients must explicitly implement quota display.

Every request validates its key against CPA's local `/v1/models` endpoint. No model generation occurs. There is no positive authorization cache: revoked keys lose access when CPA rejects them. If CPA cannot establish authorization, quota data is not returned.

Responses include `Cache-Control: no-store`, `Content-Type: application/json`, and `X-Content-Type-Options: nosniff`. Successful requests include `Retry-After: 30` as client polling guidance, not a freshness promise.

## Response

```json
{
  "schema_version": 1,
  "shared_pool": true,
  "generated_at": "2026-10-03T14:00:00Z",
  "providers": [
    {
      "provider": "codex",
      "accounts": [
        {
          "provider": "codex",
          "account_id": "acct_opaque_identifier",
          "plan": "pro",
          "windows": [
            {
              "key": "primary",
              "group": "main",
              "label": "weekly",
              "duration_seconds": 604800,
              "used_percent": 25,
              "remaining_percent": 75,
              "reset_at": "2026-10-08T00:00:00Z",
              "state": "reported",
              "source": "cpa_passive",
              "observed_at": "2026-10-03T13:59:00Z",
              "age_seconds": 60,
              "freshness": "fresh"
            }
          ],
          "routing": {
            "available": true,
            "state": "available",
            "checked_at": "2026-10-03T13:59:55Z"
          },
          "refresh": {"state": "idle"},
          "observed_at": "2026-10-03T13:59:00Z",
          "age_seconds": 60,
          "freshness": "fresh"
        }
      ],
      "summary": {
        "account_count": 1,
        "routing_available": 1,
        "routing_unavailable": 0,
        "routing_unknown": 0,
        "fresh_accounts": 1,
        "stale_accounts": 0,
        "unknown_accounts": 0
      }
    },
    {
      "provider": "claude",
      "accounts": [],
      "summary": {
        "account_count": 0,
        "routing_available": 0,
        "routing_unavailable": 0,
        "routing_unknown": 0,
        "fresh_accounts": 0,
        "stale_accounts": 0,
        "unknown_accounts": 0
      }
    }
  ],
  "snapshot_state": "available",
  "warnings": ["Shared subscription pool; percentages are not additive. Observations do not guarantee future capacity or model routing success."]
}
```

The example is illustrative, not a live subscription balance.

### Account identity and privacy

IDs are HMAC-SHA256 values incorporating provider and CPA auth index, truncated to 128 bits. Preserve the deployment salt to preserve identities. Restarting or reordering accounts does not change IDs. Replacing a CPA auth identity or rotating the salt can change IDs.

Model scopes and nonstandard quota group names are also opaque HMAC identifiers. This retains distinctions without reflecting arbitrary upstream text into public responses. `main` and `code_review` are fixed group names; scoped or named limits have stable opaque names. Model-associated observations have `scope`; these are not extra balances to sum with account-level observations. Plan names use an explicit allowlist. Unknown plan names are omitted.

### Windows and freshness

- `key`: provider window role or name; Codex `primary` is not a duration.
- `label`: `five_hour`, `daily`, `weekly`, `custom`, or `unknown_duration` according to actual duration.
- `duration_seconds`, percentages, and reset times: `null` when unavailable. A zero-duration Codex window is not treated as usable capacity.
- `source`: `cpa_passive` or `active_check`.
- `state`: `unknown`, `reported`, `warning`, `limited`, or `exhausted`. `limited` can originate from a group-level provider restriction and does not assign exhaustion to an arbitrary window.
- `freshness`: `fresh`, `stale`, or `unknown`. A passed reset deadline makes a window stale without replacing its reported usage or exhaustion state.
- Account age is conservative: based on the oldest retained window observation, including scoped observations. Inspect individual windows for precise freshness. One new window does not freshen unrelated old windows.
- `generated_at` is serialization time, never observation time. Re-reading CPA snapshots does not advance observations.

Claude passive utilization headers are fractions, multiplied by 100. Claude OAuth usage payload utilization is already a percentage, including values below 1; it is not heuristically multiplied. Codex used percentages retain their source unit. NaN, infinity, out-of-range values, and malformed numbers are not coerced to zero.

Optional `limiting_window` and `additional_usage` reflect explicit source facts only. Credits are not exposed because generic credit balances do not provide an authoritative display unit in the passive contract. No credit spending/reset endpoint is called.

### Routing and errors

Routing is a separate CPA observation. `available: null` means unknown; availability becomes unknown when the local snapshot cannot be considered current. Neither routing availability nor observed quota guarantees a model request will succeed.

`refresh.state` can be `idle`, `running`, `succeeded`, `backoff`, `external_owner`, or `ownership_unverified`. Safe refresh error codes include `rate_limited`, `upstream_authentication_failed`, `unsupported`, `invalid_observation`, `refresh_unavailable`, and `refresh_interrupted`. Last successful windows remain available after a refresh failure.

`attempted_at` and `next_attempt_at` describe attempts/budgets, not quota recovery deadlines. Backoff survives restart; provider Retry-After guidance takes precedence over the ordinary backoff cap.

The provider summaries count accounts only. There is deliberately no aggregate pool percentage.

## HTTP failures

| Status | Meaning |
| --- | --- |
| 400 | Unsupported query parameters |
| 401 | Missing, malformed, invalid, or revoked client credentials |
| 404 | Unsupported route |
| 405 | Unsupported method |
| 503 | Authorization unavailable or local request concurrency limit reached |

An authorized request can return 200 with empty/unknown data while the first snapshot is loading. Clients must distinguish unknown data from a confirmed empty or exhausted allowance.

`GET /healthz` is a minimal process liveness check, not a provider health guarantee. It is not separately routed through the gateway.
