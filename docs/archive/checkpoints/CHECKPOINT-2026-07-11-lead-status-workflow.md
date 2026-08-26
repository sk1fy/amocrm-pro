# Checkpoint: widget execution hardening and lead status workflow

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/widget-hardening-first-workflow`

Base: `2c16f27` (merged PR `#33`)

Issues: bounded hardening `#32`; workflow epic `#10`; production backlog `#21`.

Предыдущий checkpoint:
[`CHECKPOINT-2026-07-11-widget-idempotency.md`](CHECKPOINT-2026-07-11-widget-idempotency.md)

## Цель среза

Перед первым внешним amoCRM effect убрать authorization через неструктурированный
job payload, ограничить lifetime disposable JWT, добавить bounded recovery stale
admission и реализовать один реальный convergent workflow без Redis.

## Bounded hardening из #32

- Migration `000002` добавляет nullable paired actor/resource identity в `jobs`,
  actor lookup index и строгую consistency model для `idempotency_keys`.
- Completed idempotency row обязан содержать существующий job, `202` и JSON
  object; job FK теперь `ON DELETE RESTRICT`.
- Retry сверяет stored `job_id` с публичным response и под row lock reclaim-ит
  только expired row либо тот же request, застрявший в `processing` более 5 минут.
- Widget job status ищется по installation + durable actor, разрешает только
  известные action types и преобразует result в action-specific DTO. Raw
  job payload/result не возвращается.
- Worker проверяет current attempt/lease, durable actor/resource, active
  installation и active integration; непосредственно вокруг PATCH держит
  `FOR SHARE` lifecycle lock. Token lookup выполняется до lock, чтобы callback
  не требовал второй PostgreSQL connection и не блокировал reauth transition.
- `WIDGET_JWT_MAX_LIFETIME` по умолчанию равен 15 минутам; JWT с большим
  интервалом `exp - iat` отклоняется. Replay row всё ещё хранится до `exp + leeway`.

## Первый реальный workflow из #10

Endpoint `POST /api/v1/widget/actions/leads/set-status` принимает strict JSON:

```json
{"lead_id": 501, "pipeline_id": 601, "status_id": 701}
```

Контракт:

1. Admission атомарно связывает disposable JWT, hashed Idempotency-Key, durable
   actor/resource и `workflow.lead.set_status` job.
2. Worker проверяет, что verified widget actor остаётся active amoCRM admin.
3. Worker читает lead через `GET /api/v4/leads/{id}`.
4. Если pipeline/status уже совпадают, PATCH не отправляется.
5. Иначе после повторной local authorization проверки выполняется
   `PATCH /api/v4/leads/{id}` с двумя числовыми полями.
6. После неоднозначного PATCH retry снова делает GET; уже применённое состояние
   завершает job без второго PATCH.
7. Public result содержит только lead/pipeline/status IDs и truth-preserving
   `converged: true`; audit идемпотентно связывает actor, job и resource без
   полного amoCRM response или PII.

Workflow имеет только widget trigger. Созданный им `status_lead`/`update_lead`
webhook проходит существующий inbox audit path и не маршрутизируется обратно в
этот workflow, поэтому текущий vertical slice не образует петлю.

amoCRM contract сверялся с официальными страницами:

- https://www.amocrm.ru/developers/content/crm_platform/leads-api
- https://www.amocrm.ru/developers/content/crm_platform/users-api

## Integration evidence

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make config` — PASS: development Compose config валиден.
- `make integration-test` — PASS:
  - PostgreSQL 17 migration `up -> down -> concurrent up` для двух migrations;
  - race-enabled jobs/OAuth/webhook/widget/JWT tests;
  - durable actor/resource ownership и actor-scoped job lookup;
  - stale same-request processing reclaim и idempotency consistency constraint;
  - strict workflow HTTP admission;
  - real TLS HTTP stub + real amoCRM client GET/compare/PATCH;
  - active admin update и already-desired no-op;
  - ambiguous PATCH applied remotely, retry GET, ровно один PATCH;
  - installation disabled между GET и PATCH: zero PATCH;
  - concurrent disable ждёт lifecycle lock и завершается после bounded PATCH;
  - current lease/attempt fencing отклоняет stale worker после reclaim;
  - admin отозван между GET и PATCH: zero PATCH;
  - disabled integration: zero amoCRM calls;
  - non-admin actor: zero lead reads/PATCH;
  - typed result strips unknown internal fields;
  - повтор handler после успешного effect сохраняет ровно один correlated audit.
- `make test` — PASS: runtime builds, formatting, vet и
  `go test -race -count=1 ./...`.
- `make openapi-check` — PASS.
- `git diff --check` — PASS.

## Явно отложено

Issue `#32` остаётся открытым:

- browser topology и CORS/preflight policy требуют подтверждения реального
  deployment path; текущий API не добавляет broad origin allowlist;
- periodic cleanup cadence/scheduler для expired token/idempotency rows;
- полный uninstall/revocation lifecycle и его product semantics;
- общий стабильный JSON error envelope.

Issue `#10` остаётся epic: webhook-origin workflows, generalized typed registry,
effect correlation с webhook delivery и дополнительные domain actions ещё не
реализованы. Backlog `#21` не менялся.

## Resume order

1. Commit/push, открыть PR к `main`, добавить ссылки/evidence в `#32` и `#10`.
2. Дождаться всех GitHub Actions; merge самостоятельно не выполнять.
3. После merge определить browser/CORS topology и cleanup scheduler contract в
   `#32`.
4. Для следующего #10 slice выбрать webhook-origin workflow и durable
   self-effect correlation, сохраняя PostgreSQL-only архитектуру.

Checkpoint не содержит JWT, Idempotency-Key, secrets, production payloads или
PII. Commit SHA и PR URL записываются в GitHub Issues после push.
