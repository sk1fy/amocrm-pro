# Checkpoint: atomic widget action admission

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/widget-idempotency-contract`

Base: `8493d75` (merged PR `#30`)

Issue: `#31`; follow-up hardening: `#32`; parent epic: `#8`.

Предыдущий checkpoint:
[`CHECKPOINT-2026-07-11-oauth-reconcile.md`](CHECKPOINT-2026-07-11-oauth-reconcile.md)

## Цель среза

Устранить окно между расходованием disposable widget JWT и созданием job,
подключить подготовленную PostgreSQL idempotency model и доказать concurrency,
rollback и tenant/user isolation без Redis.

## Контракт

1. Middleware проверяет signature/claims, но для mutating action не расходует
   `jti` отдельно.
2. Handler принимает только пустое body и ровно один bounded
   `Idempotency-Key`; raw key хешируется SHA-256 и не сохраняется.
3. Транзакция блокирует active installation, вставляет replay record, claims
   idempotency key, создаёт job и сохраняет точный `202` response.
4. Повтор того же `jti` возвращает `401`; retry использует новый JWT и прежний
   key, получая тот же `job_id` и `Idempotency-Replayed: true`.
5. Тот же key с другим verified actor/request hash возвращает `409` и не
   создаёт job; разные installations независимы.
6. Expired key reclaim выполняется под row lock. Ошибка до commit откатывает
   token, key и job вместе.
7. Replay row хранится до `exp + leeway`, поэтому cleanup не открывает окно
   повторного использования ещё принимаемого токена.
8. Job status возвращает только `widget.ping` своей installation и verified
   user; internal и same-tenant foreign-user jobs выглядят как `404`.

## Изменения

- `jobs.Store.EnqueueTx` позволяет domain store добавлять job в caller tx.
- `widgetauth.Authenticator.Verify` и специальный verification middleware
  формируют immutable principal; обычные read endpoints по-прежнему используют
  verify-and-consume.
- `widgetapi.ActionStore` реализует installation → jti → idempotency → job lock
  order и stored response replay.
- Ping HTTP/OpenAPI contract требует `Idempotency-Key`, запрещает body и
  документирует `400/401/409/503`.
- Docker integration target включает `widgetapi` и `widgetauth`.

## Проверки через Docker

- `make integration-test` — PASS:
  - PostgreSQL 17 migration `up -> down -> concurrent up`;
  - race-enabled jobs/OAuth/webhook/widget tests;
  - 16 concurrent attempts одного jti: один success;
  - 16 fresh jti с одним key: одна job и одинаковый response;
  - synthetic job insert failure: token/key/job полностью rolled back;
  - inactive tenant rejected before consumption;
  - HTTP replay, forbidden body/multiple keys и user-owned job status;
  - installation-scoped key и expired-row reclaim.
- `make test` — PASS: runtime builds, format, vet и
  `go test -race -count=1 ./...`.
- `make openapi-check` и `git diff --check` — PASS.

## Субагент-аудит и отложенный scope

Три read-only субагента независимо проверили auth, idempotency/schema и порядок
архитектурных фаз. Их общий вывод — этот admission contract должен предшествовать
реальному workflow. Не входящие в bounded срез CORS/deployment decision, cleanup,
stale processing recovery, generalized actor ownership и worker-time permission
check сохранены в Issue `#32`.

## Resume order

1. Commit/push, создать PR с `Closes #31`, дождаться всех GitHub Actions.
2. После merge добавить evidence в `#8`; не закрывать epic до решения settings и
   browser/cleanup boundary.
3. Выбрать один реальный amoCRM action для Issue `#10`, определить permission и
   external-effect idempotency до реализации.
4. Redis не добавлять без измерений и ADR.

Checkpoint не содержит JWT, raw idempotency keys, secrets, production payloads
или PII. Commit SHA и PR URL записываются в Issue `#31` после push.
