# quota-api — Concept Specification

## Purpose

Provide coding clients with a safe, consistent view of Codex and Claude subscription quotas through `GET /v1/quota`, using the same API key they already use for model requests.

This is an independent companion service to CLIProxyAPI (CPA), not a native plugin or a fork of CPA. It reports quota information; it does not perform generation or control subscriptions.

## Agreed Product Decisions

- Implement the service in Go.
- Run it in its own Docker container alongside CPA.
- Expose `/v1/quota` through the existing gateway, including its personal aliases, while preserving their existing access restrictions.
- Accept all current valid CPA client API keys; do not require another client credential.
- Return JSON with separate anonymous account records.
- Support Codex and Claude only.
- Do not include a web interface, historical analytics, or notifications in the first version.

This document defines the product concept and expected behavior, not implementation details or an exact response schema. Deployment and implementation are separate work.

## Existing Environment

CPA currently serves a shared subscription pool. Personal gateway addresses do not represent separate subscription allowances. Clients must see that the quota belongs to the shared pool, not to the individual calling user.

CPA already retains quota observations from normal provider responses, including response headers and Codex WebSocket events. Reading those stored observations does not itself require a provider quota request.

CPA Usage Keeper is a separate application that supports quota inspection and refresh in addition to usage analytics. It must not be treated as merely a token counter, nor assumed to expose an appropriate public client quota API.

The existing model request path and other companion services remain unchanged.

## Client Experience

An authenticated client asks for quota status and receives a view of the shared pool, grouped by provider and separated by account.

Each account has a stable opaque identifier that reveals no personal identity and does not change just because the service restarts or account ordering changes.

The response conveys:

- Provider and anonymous account identifier.
- Subscription plan when reliably known and safe to disclose.
- Applicable quota windows and their actual duration when known.
- Used and remaining percentages where reported or safely derivable.
- Reset time when known.
- Observation time and freshness for each account or window.
- CPA routing availability separately from quota consumption.
- A safe explanation when information is unavailable or refresh fails.

The endpoint is a custom quota interface, not a standard OpenAI API capability. Clients need explicit support for consuming it; existing coding clients will not automatically display it.

## Access and Privacy

All valid CPA client keys can read the shared-pool view. Invalid or revoked keys must not retain indefinite access through cached authorization. Personal gateway restrictions must still apply.

Client credentials provide read-only quota access, never administrative authority. Administrative credentials stay server-side.

Never expose OAuth tokens, client keys, emails, upstream account identifiers, credential filenames, filesystem paths, internal addresses, raw provider responses, or raw diagnostic messages.

Do not record secrets in service logs. Public account identifiers must not be reversible representations of personal information.

If access validity cannot be established, deny access rather than exposing data anonymously.

## Freshness Without Excessive Provider Traffic

Use a passive-first, controlled-refresh approach.

### Passive observations first

Prefer recent quota observations already collected by CPA during normal model traffic. Re-reading a cached observation must not make its observation timestamp newer.

A client request should normally return a cached view, without contacting Codex or Claude.

### Refresh only when useful

When observations are missing or stale and clients are using the quota service, allow bounded background quota checks. Do not generate model requests merely to obtain headers.

Refresh work is shared across clients and limited per upstream account. Multiple clients, aliases, or simultaneous requests must not multiply provider checks. Frequent endpoint polling must not override the refresh limits.

Prefer fresh passive observations over unnecessary scheduled checks. Reduce or stop active polling when the service has no interested clients. Returning to active use may schedule a refresh, but must still respect provider backoff.

Respect provider rate limits and retry guidance. After repeated failures, progressively reduce refresh frequency. For an unsupported or unreliable quota endpoint, return stale or unknown data rather than repeatedly retrying.

Coordinate refresh ownership with Usage Keeper. Reuse a suitable existing observation where possible or designate one component to perform active checks; do not introduce two independent polling loops for the same account.

### Practical freshness policy

Local snapshot reads can be frequent because they do not contact providers. Active provider checks should operate on a minutes-scale budget, not on every client poll. Provider-specific intervals and stale thresholds remain configurable and must be validated before deployment; this specification does not promise a provider-safe universal interval.

A known reset deadline may make a refresh useful, but must not override rate limiting. A passed reset time is not evidence that quota has actually recovered.

Active checks must preserve the account's existing connection policy. They must not change credentials, routing configuration, subscription settings, or spend reset credits.

## Correct Interpretation

### Codex

Support all applicable reported quota windows, including named or model-specific limits where available. Determine window labels from their actual duration: primary is not necessarily five hours, and secondary is not necessarily a week.

Fork extension: optionally expose banked reset inventory from the fixed
read-only inventory GET, independently of usage polling ownership. Retain only
the reported available count and grant/expiry dates for available entries.
Never retain redemption IDs or offer an operation that consumes a reset.
Cache observations with a persisted, interest-gated budget of at least 15m per
account and failure backoff. Missing inventory is unknown, not zero.

Preserve known distinctions between quota groups. Show credits only with a known meaning and unit; do not convert them into dollars, tokens, or extra percentage capacity without authoritative information.

### Claude

Support the five-hour and weekly windows, plus scoped limits when available. Normalize provider-specific fractional values into a consistent percentage representation.

Report the limiting window or additional usage state only when it is supported by the source data. Do not infer subscription allowance from API token counts.

### Shared pool

Do not add or average percentages across accounts to invent a single pool balance. Different plans and limits may have different capacities.

A pool summary may report account counts and whether CPA currently has available accounts, with unknown and stale states preserved. Such a summary is not a guarantee that a particular model request will succeed.

### Unknown is not zero

Missing windows, unavailable observations, and failed refreshes must not appear as zero remaining quota, zero usage, or unlimited capacity. Optional information remains explicitly unknown or absent.

Routing availability, quota exhaustion, authentication problems, and refresh errors are different states and must remain distinguishable.

## Failure Behavior

Quota service failures must not interrupt model generation. The quota endpoint must not become a dependency of the model request path.

If CPA or a provider is temporarily unavailable, retain the last successful observation and identify its age and stale status. If no observation exists, report unknown. An error for one account must not hide valid data for other accounts.

Freshness is based on the original observation, not response generation time. No response guarantees future capacity: other clients and activity outside this gateway may consume the same subscription.

Clients should tolerate quota endpoint failure, avoid excessive polling, and display stale or unknown information honestly.

## First-Version Boundaries

Excluded:

- Web UI, history, forecasts, and notifications.
- Per-user quota allocation or enforcement.
- Changes to CPA routing, account selection, or credential management.
- Subscription upgrades, quota resets, or credit spending.
- Conversion of token usage into an asserted subscription balance.
- Public access to CPA Management API or Keeper administrative routes.
- Caller-controlled provider requests or unrestricted force-refresh operations.
- Automatic integration into every coding client.

## Acceptance Criteria

- A current valid CPA client key can read `/v1/quota`; invalid keys cannot.
- Clients see separate anonymous Codex and Claude accounts and are told that the pool is shared.
- Windows retain their meaning, duration, reset information, and observation age.
- CPA routing availability is distinct from quota state.
- Fresh cached observations do not trigger active provider checks.
- Parallel clients cannot cause duplicate or unbounded account refreshes.
- Keeper and quota-api do not independently over-poll the same accounts.
- Stale data, missing data, refresh errors, and actual exhaustion are distinguishable.
- Secrets and identifying account information never appear in public responses.
- Quota service failure does not affect normal model requests.

## Evidence Informing the Concept

- [CPA v7.3.16 auth-file observations](https://github.com/router-for-me/CLIProxyAPI/blob/v7.3.16/internal/api/handlers/management/auth_files.go).
- [CPA v7.3.16 Codex quota event handling](https://github.com/router-for-me/CLIProxyAPI/blob/v7.3.16/internal/runtime/executor/helps/codex_quota.go).
- [Deployed Keeper revision: quota service](https://github.com/Willxup/cpa-usage-keeper/blob/7b1a5477e9240e1e0478a54abefbb5d7462d0bb0/internal/quota/service.go).

Repository capabilities do not prove that every optional refresh feature is enabled on the server. Verify existing refresh ownership and provider behavior before implementation and deployment.
