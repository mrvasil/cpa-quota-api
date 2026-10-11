# Deployment and operations

## Topology

- One container next to CPA on a shared Docker network. CPA is reached at `CPA_URL` on that network.
- Runs as UID/GID 10001 with a read-only root filesystem, dropped capabilities and `no-new-privileges`.
- Publish the HTTP port only on loopback (`127.0.0.1:8082`) and expose `/v1/quota` through the gateway that already fronts CPA.
- Persistent state in `STATE_DIR` (mode 0700, owned by 10001). Secrets in an env file with mode 0600. Never commit either.
- Exactly one replica per state directory. A file lock prevents two processes from sharing one state directory; there is no distributed refresh lease across hosts.

## Gateway

Add a dedicated `/v1/quota` handler to every gateway host that should serve it, after any per-key access restrictions and before the generic CPA proxy, so the endpoint keeps the same access rules as model traffic. Validate the new gateway configuration before reloading and keep a backup. Do not route CPA management or Usage Keeper administrative paths publicly.

## Refresh ownership

Only one component may actively poll provider quota endpoints.

- `REFRESH_OWNER=keeper` (default): this service only reads passive CPA observations.
- `REFRESH_OWNER=quota-api`: this service runs one serialized, budgeted refresh worker. Before each active attempt it verifies through Usage Keeper's authenticated settings that Keeper auto-refresh is disabled. Failed authentication, malformed settings, an unavailable Keeper or enabled Keeper polling block active checks; the guard is retried at most once per minute regardless of client polling.

To move ownership to Keeper: set `REFRESH_OWNER=keeper`, recreate this container, then enable Keeper polling. Reverse the order to move it back.

Clients cannot pass provider URLs, account selectors, force-refresh flags, proxy overrides or credit/reset operations. Active requests are fixed GET quota reads sent through CPA's management `api-call`, so CPA applies its existing account and proxy policy and substitutes the token. This service never loads OAuth credential files.

## Freshness policy

| Setting | Default |
| --- | --- |
| Passive CPA snapshot interval while clients are interested | 15 seconds |
| Interest lifetime after the last authorized client request | 2 minutes |
| Codex / Claude active minimum interval, per account | 15 minutes |
| Codex / Claude stale threshold | 10 minutes |
| Maximum ordinary failure backoff | 24 hours |

Fresh unscoped observations suppress active checks. Repeated failures increase backoff; unsupported endpoints, malformed payloads and authentication errors use the long backoff. `Retry-After` from providers can postpone checks beyond the cap. Neither client polling nor a reset deadline bypasses the budget. Backoff is persisted before and after each attempt and survives restarts.

These are conservative defaults, not provider-approved universal intervals. Watch refresh states before tightening them.

## Upgrade

```bash
docker compose pull
docker compose up -d
docker compose ps
```

Keep the env file, salt and state directory across upgrades. Losing the salt changes all account IDs; losing state discards retained observations and backoff.

## Verification checklist

After deploying or changing the gateway:

1. Every valid CPA client key can read `/v1/quota`; missing and invalid keys get 401; both `Authorization: Bearer` and `X-API-Key` work.
2. Per-key gateway restrictions still apply to `/v1/quota` exactly as to model routes.
3. The public JSON contains no client keys, management or Keeper credentials, salt, raw auth indexes, emails, credential file names or paths.
4. Account IDs are unchanged after a container restart.
5. Stopping this container leaves CPA `/v1/models` working and makes only `/v1/quota` fail (gateway error, no fall-through to CPA).

The container healthcheck is process-only; an upstream quota failure can legitimately coexist with a healthy process and stale data.
