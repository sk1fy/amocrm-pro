# Project context

Устойчивый snapshot проекта. Это не backlog: текущие scope и status находятся
в GitHub Issues. При расхождении источником реализованного поведения являются
код, миграции и runtime-конфигурация; источником архитектурного намерения — ADR.

## Snapshot

- Проверено: 2026-08-31 (Europe/Moscow).
- Branch: `main`.
- `BUG-010` implementation commit: `53c78f5`.
- Последнее изменение runtime-кода: capacity slice PR #54, merge commit `9e9d5ba`.
- Предыдущий main CI: success, run `32957486277`; `BUG-010` CI:
  run `33339274655`.
- `BUG-010` Docker gate: unit/race, OpenAPI, PostgreSQL integration and
  api/worker/migrate builds pass; real private-widget E2E pending.
- Runtime: Go 1.25, PostgreSQL 17 Alpine.
- Миграции: шесть обратимых versioned migrations.
- Redis: отсутствует по ADR-0001.
- Стадия: functional MVP реализован; production hardening не завершён.

## Runtime boundaries

- `amocrm-api` — OAuth, widget API, webhook ingress и public liveness;
- отдельный API management listener — readiness и metrics;
- `amocrm-worker` — PostgreSQL jobs, amoCRM API, workflows и cleanup;
- `amocrm-migrate` — короткоживущая migration utility;
- PostgreSQL — system of record, inbox, queue, replay/idempotency и audit.

API и worker являются раздельными deployment units одного Go-модуля. Все
проектные Go/PostgreSQL операции выполняются через Docker/Make.

## Implemented capabilities

### OAuth and amoCRM client

- hashed one-time OAuth state и code exchange;
- atomic installation/credentials/reconcile/audit persistence;
- AES-256-GCM keyring с versioned AAD;
- encrypted integration secret и access/refresh tokens;
- version-fenced refresh, один retry после `401`, `reauth_required` после
  повторного `401`;
- allowlisted HTTPS account domains и redirect refusal;
- специализированные API v4 методы account/users/leads/webhooks;
- per-integration/per-account process-local outbound rate limiting.

### Widget API

- strict HS256 claims validation и maximum token lifetime;
- primary `X-Auth-Token` и compatible `Authorization: Bearer`, с fail-closed
  rejection при одновременной или multi-valued подаче;
- tenant lookup только по `client_uuid + account_id` из verified JWT;
- atomic jti consumption, Idempotency-Key outcome и job enqueue;
- actor/resource ownership в typed job columns;
- bootstrap, ping, lead status action, rule configure и actor-scoped job status;
- live amoCRM admin re-check перед privileged mutation;
- tenant-bound CORS и exact origin/issuer binding.

### Webhooks and workflows

- secret per-installation webhook URL и account verification;
- bounded form-urlencoded ingress с durable commit до `204`;
- process-local ingress limiters с bounded tenant cache;
- normalized inbox events, durable tombstones и replay protection;
- configurable 30-day default raw delivery/inbox retention;
- lead-status widget and webhook-origin workflows;
- source-state fence, GET/compare/PATCH convergence и durable outbound effects;
- semantic self-effect correlation и loop prevention;
- async lead-status rule configuration с revision CAS and durable receipt.

### Jobs and operations

- PostgreSQL `SKIP LOCKED` queue, leases, heartbeat and attempt fencing;
- retry/backoff, permanent failure, dead state and panic recovery;
- bounded expired-lease reaping independent from ready claim size;
- capacity evidence at 1 000 and 100 000 expired jobs;
- exact ready/scheduled/expired backlog gauges;
- bounded replica-safe maintenance under PostgreSQL advisory lock;
- guarded destructive migration rollback and operator runbook.

## Confirmed gaps

1. Реальный installed-private-widget E2E для `X-Auth-Token` ещё не выполнен;
   автоматизированная совместимость реализована в `BUG-010` / #55.
2. Token refresh holds a PostgreSQL transaction/row lock across an external
   amoCRM OAuth request.
3. `oauth_states` has no cleanup and `/oauth/start` has no ingress rate limit.
4. Jobs, attempts, audit, tombstones, workflow runs and outbound effects have
   no finite retention policy.
5. Webhook duplicate/stale removal, key rotation, unregister and complete
   uninstall/revocation lifecycle are unfinished.
6. Widget settings and stable JSON error contracts are unfinished.
7. Dashboards, alerts, SLO, backup/restore rehearsal, least-privilege DB roles,
   KMS boundary and production security/capacity program remain open.

## Invariants

- Не добавлять Redis без отдельного ADR и измеряемой необходимости.
- Webhook ingress не выполняет внешних calls и отвечает `2xx` только после
  durable PostgreSQL commit.
- Секреты, JWT, OAuth tokens, raw production payloads и PII не размещаются в
  repository, Issues, logs или fixtures.
- API и worker сохраняют независимый lifecycle и deployment boundary.
- Исторические checkpoints не используются как текущий resume order.

## Navigation

- Current architecture: [`../architecture.md`](../architecture.md)
- Phase mapping and recovery: [`ROADMAP.md`](ROADMAP.md)
- Defects: [`BUGS.md`](BUGS.md)
- Program status: [GitHub Issue #12](https://github.com/sk1fy/amocrm-pro/issues/12)
- Historical evidence: [`../archive/checkpoints/`](../archive/checkpoints/)
