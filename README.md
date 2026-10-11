# cpa-quota-api

Read-only companion service for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) that exposes the Codex and Claude subscription quota of a shared account pool at `GET /v1/quota`. Clients authenticate with the same CPA client key they already use for model requests.

It runs as its own container next to CPA, so a quota failure can never break model traffic. It is not a CPA plugin and does not modify CPA.

The service never exposes upstream account identifiers, credentials, emails, paths, raw provider payloads or diagnostic details. Account IDs are HMAC-derived from the CPA auth index and a deployment-only salt.

Clients: [`pi-cpa-quota`](https://github.com/umbra2728/pi-cpa-quota) shows the quota in the [pi](https://pi.dev) coding agent.

## Response contract

- `shared_pool: true` and separate `providers[].accounts[]` records with opaque, stable `account_id` values.
- Provider windows with source duration, percentages, reset time, original observation time, age, freshness and state.
- CPA routing state separately from quota state.
- Refresh state, backoff, ownership conflicts and safe warnings.

Percentages are never combined across accounts. Missing or invalid values remain unknown. Reset deadlines do not imply recovery. Full schema: [docs/API.md](docs/API.md). Design rationale: [SPEC.md](SPEC.md).

## Run

```bash
docker network create cpa                        # or reuse the network CPA is on
cp .env.example cpa-quota-api.env && chmod 600 cpa-quota-api.env
# fill in CPA_MANAGEMENT_KEY and ACCOUNT_ID_SALT (openssl rand -hex 32)
mkdir -p data && sudo chown 10001:10001 data && chmod 700 data
docker compose up -d
```

Images are published to `ghcr.io/umbra2728/cpa-quota-api` (`latest` and version tags, `linux/amd64` and `linux/arm64`).

Then route `/v1/quota` to the service in front of CPA, before the generic CPA proxy, keeping any existing per-key restrictions. Caddy example:

```caddyfile
handle /v1/quota {
    reverse_proxy cpa-quota-api:8080
}
```

Never expose CPA management routes or Usage Keeper administrative routes through the gateway. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for refresh ownership, freshness policy and verification.

## Configuration

Environment variables (secrets also accept a `_FILE` variant):

| Variable | Default | Meaning |
|---|---|---|
| `CPA_URL` | `http://cli-proxy-api:8317` | CPA base URL on the internal network |
| `CPA_MANAGEMENT_KEY` | — | server-side CPA management reads |
| `ACCOUNT_ID_SALT` | — | at least 32 random bytes, hex encoded; keep it to keep account IDs stable |
| `REFRESH_OWNER` | `keeper` | `keeper` disables active checks here; `quota-api` enables one shared, bounded refresh worker after confirming Usage Keeper auto-refresh is disabled |
| `KEEPER_URL`, `KEEPER_PASSWORD` | — | required when `REFRESH_OWNER=quota-api` |
| `CODEX_REFRESH_INTERVAL`, `CLAUDE_REFRESH_INTERVAL` | `15m` | minimum per-account active check interval (at least 1m) |
| `CODEX_STALE_AFTER`, `CLAUDE_STALE_AFTER` | `10m` | when an observation counts as stale |
| `MAX_BACKOFF` | `24h` | cap for ordinary failure backoff |
| `SNAPSHOT_EVERY`, `INTEREST_FOR` | `15s`, `2m` | passive CPA snapshot cadence while clients are interested |
| `STATE_DIR`, `LISTEN_ADDR` | `/data`, `:8080` | persistent state and listen address |

Never enable active refresh in both this service and Usage Keeper.

## Operational guarantees

Authorization is checked against CPA `/v1/models` for every request; no stale authorization is accepted after a failed revalidation. Passive reads come from CPA's existing observations and do not contact providers. Refreshes are serialized globally, limited per account, interest-gated, persisted with backoff, and stopped when the ownership guard cannot prove Keeper polling is disabled.

## Development

```bash
gofmt -l .
go vet ./...
go test -race ./...
docker build -t cpa-quota-api:local .
```

CI runs the checks on every push; pushing a `v*` tag builds and publishes the multi-arch image to GHCR.
