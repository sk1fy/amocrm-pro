# Current architecture

Этот документ описывает фактически реализованную архитектуру `main`. Исходные
design proposals сохранены в [`archive/`](archive/) и не являются runtime
контрактом.

## Runtime topology

```text
Browser widget                 amoCRM
      |                     /          \
      | HTTPS              OAuth      Webhooks
      v                       \          /
                        amocrm-api
                             |
                    durable PostgreSQL commit
                             |
                        amocrm-worker
                             |
                       amoCRM API v4
```

Сервисы собираются из одного Go-модуля, но запускаются независимо:

- `amocrm-api` принимает OAuth callback, widget requests и webhooks;
- `amocrm-worker` выполняет jobs, amoCRM API calls, workflow и cleanup;
- `amocrm-migrate` является короткоживущей deployment utility;
- PostgreSQL 17 — единственное durable runtime-хранилище.

Redis отсутствует по [ADR-0001](adr/0001-postgresql-without-redis.md).

## Data ownership

Основные durable сущности:

- `integrations` — client UUID, encrypted client secret, redirect URI и настройки;
- `installations` — tenant/account mapping, domain и lifecycle status;
- `oauth_credentials` — encrypted access/refresh tokens и `token_version`;
- `webhook_deliveries` / `inbox_events` — raw delivery и normalized events;
- `webhook_event_tombstones` — replay identity после удаления raw payload;
- `jobs` / `job_attempts` — PostgreSQL queue, leases и история попыток;
- `workflow_runs` / `outbound_effects` — workflow identity и side-effect history;
- `used_widget_tokens` / `idempotency_keys` — replay и request idempotency;
- `audit_log` — redacted correlated audit trail.

## OAuth flow

1. Operator bootstraps integration metadata from environment.
2. `/oauth/amocrm/start` creates a hashed one-time state and redirects to amoCRM.
3. `/oauth/amocrm/callback` consumes state, exchanges code and reads account data.
4. Installation, encrypted credentials, webhook intent, audit and reconcile job
   are committed atomically.
5. Worker reconciles the desired webhook subscription.

Token refresh is version-fenced and a repeated `401` cannot rotate a token that
another caller already refreshed. Known availability risk: the current refresh
implementation holds a PostgreSQL transaction across the external OAuth call.

## Widget request flow

Intended browser flow:

```text
this.$authorizedAjax()
  -> disposable amoCRM JWT
  -> tenant/user verification
  -> atomic jti + Idempotency-Key + job commit
  -> 202 with job_id
  -> worker execution
  -> actor-scoped job status
```

Claims bind a request to `client_uuid`, `account_id`, `user_id`, issuer and
audience. Tenant identity never comes from the action JSON body.

There is a current integration gap: amoCRM sends the disposable token in
`X-Auth-Token`, while the implementation only accepts `Authorization: Bearer`
and does not allow `X-Auth-Token` in CORS. See `BUG-010` in
[`project-memory/BUGS.md`](project-memory/BUGS.md).

## Webhook flow

1. amoCRM posts form-urlencoded payload to a per-installation secret URL.
2. API applies global/per-installation process-local limits, body limit and
   account verification.
3. Delivery and parse job commit before API returns `204`.
4. Worker parses one delivery into normalized inbox events and processing jobs.
5. Tombstones prevent historical replay after raw payload retention expires.

No external call occurs in webhook ingress.

## Jobs and effects

Workers claim ready jobs with `FOR UPDATE SKIP LOCKED`, leases and attempt
fencing. Handlers classify permanent/retryable failures, heartbeat active work,
recover expired leases in bounded batches and prevent stale workers from
finalizing reclaimed jobs.

The lead-status workflow performs GET/compare/PATCH. Outbound intent is durable
before PATCH, retries compare remote state, and matching incoming webhooks mark
the effect observed rather than starting a loop.

## Network and operational boundaries

- Public API listener: OAuth, widget, webhook and `/live`.
- API management listener: `/live`, PostgreSQL `/ready` and `/metrics`.
- Worker listener: internal health and metrics only.
- Account domains are restricted to supported amoCRM/Kommo HTTPS suffixes.
- Runtime redirects are disabled for external amoCRM clients.
- All builds, tests, migrations and PostgreSQL operations use Docker/Make.

## Known incomplete areas

- real browser `X-Auth-Token` compatibility;
- OAuth state cleanup and OAuth ingress rate limiting;
- token refresh without an external call inside a DB transaction;
- finite retention for jobs/audit/tombstones/workflow/effects;
- complete webhook rotate/unregister and uninstall/revocation lifecycle;
- widget settings endpoint and stable JSON errors;
- dashboards, alerts, SLO, backup/restore and production security hardening.
