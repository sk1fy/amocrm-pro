# Checkpoint: widget browser, cleanup, and webhook effect correlation

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/webhook-correlation-retention`

Base: `eda364e` (merged PR `#34`)

Issues: browser/retention hardening `#32`; workflow epic `#10`; production
retention backlog `#21`.

Предыдущий checkpoint:
[`CHECKPOINT-2026-07-11-lead-status-workflow.md`](CHECKPOINT-2026-07-11-lead-status-workflow.md)

## Цель среза

Зафиксировать реальную browser/CORS topology, назначить PostgreSQL-only owner
для cleanup одноразовых widget records и добавить первый webhook-origin
workflow с durable deduplication и self-effect correlation без Redis.

## Browser/CORS contract

Поддерживаемая topology — прямой HTTPS вызов публичного `amocrm-api` из JS,
который amoCRM загружает в browser account page.

- CORS middleware смонтирован только на `/api/v1/widget/*`.
- Preflight без JWT отражает только canonical HTTPS `Origin`, найденный у active
  installation активной integration.
- Разрешены только GET/POST и `Authorization`, `Content-Type`,
  `Idempotency-Key`, `X-Request-ID`; wildcard и credentials запрещены.
- Actual browser request после JWT validation обязан иметь точный
  `Origin == principal.Issuer`.
- Non-browser запрос без `Origin` сохраняет прежний контракт; OAuth, hooks,
  health и metrics не получают widget CORS headers.

Решение и deployment consequences описаны в
[`ADR 0004`](../adr/0004-widget-browser-and-cleanup-contract.md).

## Cleanup scheduler and retention

`amocrm-worker` запускает один immediate cleanup pass, затем periodic passes.
Несколько replicas координируются transaction-scoped PostgreSQL advisory lock.
Каждый pass ограничен batch size и maximum batch count, использует
`FOR UPDATE SKIP LOCKED` и PostgreSQL clock.

Default contract:

- interval `15m`;
- timeout `30s`;
- safety margin `5m`;
- batch size `500`;
- максимум `20` batches на таблицу за pass.

`used_widget_tokens.expires_at` уже содержит `JWT exp + validation leeway`,
поэтому строка удаляется только при строгом
`expires_at < database_now - safety_margin`. Idempotency rows любого состояния
удаляются по той же границе после их 24h request TTL. Job, связанный с completed
idempotency result, не удаляется. Пропущенный pass увеличивает retention, но не
сокращает safety window.

## Webhook-origin workflow and correlation

Migration `000003` добавляет:

- `webhook_event_tombstones` — compact installation-scoped dedup hashes,
  независимые от removable raw delivery/inbox payload;
- typed opt-in `lead_status_workflow_rules` с обязательным `source != target`;
- tenant-consistent `workflow_runs` с unique origin hash;
- `outbound_effects` с prepared/applied/uncertain/observed lifecycle и exact
  desired-state fingerprint;
- correlation link от inbox event к effect и индексы для active lookup/CORS/cleanup.

`status_lead` source event атомарно создаёт один run и
`workflow.lead.status_transition` job. Worker проверяет current lease, active
tenant и durable run/job ownership, затем делает GET/compare/PATCH. Effect intent
коммитится до PATCH. Поэтому webhook, пришедший во время remote call или после
ambiguous response, может перевести effect в `observed`, пометить inbox event
`ignored` и не создать новый workflow run. Retry видит уже применённый target
через GET и не отправляет второй PATCH.

amoCRM не возвращает caller operation ID, поэтому correlation семантическая:
installation + lead + exact desired pipeline/status + bounded DB receive-time
window. Для выбранного convergent workflow совпавший человеческий transition
безвредно подавляется как уже достигнутый target. Ограничение зафиксировано в
[`ADR 0005`](../adr/0005-webhook-workflow-effect-correlation.md).

## Integration evidence

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: runtime builds, formatting, vet и race-enabled tests.
- `make integration-test` — PASS:
  - PostgreSQL 17 migration `up -> down -> concurrent up` для трёх migrations;
  - strict cleanup boundary, all idempotency states, retained jobs, bounded
    batches и concurrent advisory-lock skip;
  - active installation/integration CORS lookup и strict preflight/issuer binding;
  - durable tombstone replay suppression и один workflow run/job;
  - source webhook -> one convergent PATCH -> applied effect;
  - target webhook -> observed effect + ignored event + zero recursive run;
  - widget-origin effect correlation;
  - ambiguous widget PATCH сохраняет один effect, retry GET и ровно один PATCH;
  - существующие jobs/OAuth/webhook/widget authorization tests.
- `git diff --check` — PASS.

Первый integration run обнаружил только некорректную test fixture chronology:
expired token был создан позже своего expiry и schema правильно его отвергла.
Fixture исправлена так, чтобы `created_at < expires_at`; полный gate после этого
прошёл на чистой PostgreSQL.

## Явно отложено

- UI/API CRUD и product ownership для `lead_status_workflow_rules`;
- raw delivery/inbox cleanup policy и возможная finite tombstone retention;
- cleanup/backlog metrics, SLO/alerts и load tests из `#21`;
- stable JSON error envelope и полный uninstall/revocation lifecycle из `#32`;
- generalized workflow registry и non-convergent effect correlation.

## Resume order

1. Commit/push, открыть PR к `main`, синхронизировать evidence в `#32`, `#10`, `#21`.
2. Дождаться всех GitHub Actions; merge самостоятельно не выполнять.
3. Следующим bounded slice выбрать rule management либо raw payload retention,
   сохраняя tombstones/runs/effects дольше удаляемых payload.

Checkpoint не содержит JWT, Idempotency-Key, secrets, production payloads или PII.
