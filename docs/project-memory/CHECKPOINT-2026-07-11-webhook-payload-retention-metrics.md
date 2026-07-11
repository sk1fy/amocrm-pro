# Checkpoint: webhook payload retention and workflow metrics

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/raw-webhook-retention-metrics`

Base: `0602f25` (merged PR `#35`)

Issues: retention/production hardening `#21`; workflow epic `#10`; widget
hardening context `#32`.

Предыдущий checkpoint:
[`CHECKPOINT-2026-07-11-webhook-correlation-retention.md`](CHECKPOINT-2026-07-11-webhook-correlation-retention.md)

## Выбор bounded slice

Выбран raw webhook delivery/inbox retention. Read-only subagent audits обоих
кандидатов подтвердили, что rule-management требует отдельного решения по
admin principal, async configure job и revision/CAS semantics. Retention уже
имеет безопасную границу благодаря tombstones и независимой workflow history.

## Retention contract

- `WEBHOOK_INBOX_RETENTION` и `WEBHOOK_DELIVERY_RETENTION` по умолчанию `720h`.
- Terminal inbox events (`processed`, `failed`, `dead`, `ignored`) удаляются по
  строгой границе `updated_at < database_now - retention`.
- Terminal deliveries (`parsed`, `invalid`, `failed`) удаляются после inbox и
  только если не имеют оставшихся inbox children.
- Pass остаётся под PostgreSQL advisory lock, timeout, bounded batches и
  `FOR UPDATE SKIP LOCKED`; Redis не добавлен.
- Tombstones, workflow runs, outbound effects, jobs и audit cleanup не
  затрагивает. `workflow_runs.origin_event_id` становится `NULL`, а copied
  origin hash и effect history остаются.

ADR: [`0006`](../adr/0006-webhook-payload-retention-and-metrics.md).

## Metrics

Worker использует собственный Prometheus registry и публикует:

- `amocrm_cleanup_passes_total{outcome}`;
- `amocrm_cleanup_duration_seconds`;
- `amocrm_cleanup_rows_deleted_total{record}`;
- `amocrm_cleanup_batch_limit_total{record}`;
- `amocrm_workflow_routes_total{workflow,disposition}`;
- `amocrm_workflow_jobs_total{workflow,outcome}`;
- `amocrm_workflow_duration_seconds{workflow,outcome}`.

Labels ограничены типами workflow/outcome/record; tenant, resource, payload и
error text в labels не попадают.

## Integration evidence

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make test` — PASS: runtime builds, formatting, vet и race-enabled tests.
- `make integration-test` — PASS после исправления только chronology/retention
  fixtures: migration `up -> down -> concurrent up` для четырёх migrations;
  terminal-only status matrix; strict retention; inbox-before-delivery;
  retained pending child; retained tombstone/run/effect/job; nullable origin
  event link; replay после payload deletion создаёт zero inbox events/jobs;
  существующие jobs/OAuth/webhook/widget/workflow tests.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `git diff --check` — PASS.

## Явно отложено

- finite retention для tombstones/workflow runs/outbound effects/audit/jobs;
- exact backlog gauges, capacity/load tests, dashboards, alerts и SLO;
- rule-management configure contract с admin re-check и revision/CAS;
- stable JSON error envelope и uninstall/revocation lifecycle из `#32`.

## Resume order

1. Дождаться CI текущего PR; merge самостоятельно не выполнять.
2. Следующий bounded slice — async rule configure contract после фиксации
   management principal и revision semantics либо load/SLO hardening из `#21`.

Checkpoint не содержит secrets, production payloads или PII.
