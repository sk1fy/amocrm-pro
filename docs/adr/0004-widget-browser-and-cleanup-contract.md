# ADR 0004: direct widget browser access and bounded PostgreSQL cleanup

- Status: accepted
- Date: 2026-07-11
- Issues: #32, #21

## Context

amoCRM loads widget JavaScript into the account browser page. Calls from that
page to this service therefore cross an origin boundary and use headers such as
`Authorization`, `Content-Type`, and `Idempotency-Key`, which require a browser
preflight. The API previously had no explicit preflight contract.

Disposable JWT replay rows and widget idempotency rows also have finite safety
windows, but no process owned their physical cleanup. The service must remain
PostgreSQL-only and may run more than one worker replica.

## Decision

The supported browser topology is a direct HTTPS call from an amoCRM account
page to the public `amocrm-api`. CORS is application-owned and scoped only to
`/api/v1/widget/*`:

- never use `*` and never enable credentials;
- preflight reflects only a normalized HTTPS origin that belongs to an active
  installation whose integration is also active;
- allow only the widget methods and request headers used by the public contract;
- actual browser requests additionally require their normalized `Origin` to
  equal the issuer origin of the verified disposable JWT;
- requests without `Origin` remain available to non-browser clients, while
  OAuth, webhook and system routes never receive widget CORS headers.

Cleanup is a best-effort periodic loop inside `amocrm-worker`, not a durable
queue job. It runs once at startup and then on a configurable interval. Each run:

- obtains a PostgreSQL transaction-scoped advisory lock, so worker replicas do
  not perform the same maintenance pass;
- deletes in bounded `FOR UPDATE SKIP LOCKED` batches with a run limit;
- uses PostgreSQL time as the authority;
- deletes `used_widget_tokens` only after stored `expires_at` plus a nonnegative
  safety margin. Stored expiry already includes JWT validation leeway;
- deletes any idempotency state only after its configured request TTL plus the
  same safety margin. Stale but unexpired `processing` rows remain recoverable
  by the request path;
- treats a missed run as storage delay only, never as a correctness failure.

The initial cleanup scope is deliberately limited to widget token and
idempotency rows. Webhook deliveries and inbox events are not deleted until
their deduplication identity has an independent durable tombstone.

## Consequences

Browser policy is tenant-bound instead of being a broad domain allowlist.
Preflights require a small indexed PostgreSQL lookup. Deployments must route
OPTIONS requests to the API unchanged.

Cleanup is restart-safe, horizontally safe, and bounded, but is not guaranteed
to run at an exact wall-clock instant. Retention may exceed the minimum safety
window by the scheduler interval, margin, or backlog; it must never be shorter.
