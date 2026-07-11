# Checkpoint: repeatable PostgreSQL integration gate

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/amocrm-backend-foundation`

Предыдущий checkpoint: [`CHECKPOINT-2026-07-10.md`](CHECKPOINT-2026-07-10.md)

## Цель среза

Закрыть validation gap из Issue `#20`: DB integration tests больше не должны
молча пропускаться в обычном workflow, а migration cycle и destructive test
reset должны выполняться только в отдельной PostgreSQL.

## Изменения

- `docker-compose.test.yml` создаёт отдельные test services/network/volume и
  БД `amocrm_test`; development Compose и его volume не используются.
- Docker target `integration-test` запускает race-enabled tests пакетов jobs
  и webhook.
- `make integration-test` гарантированно очищает test project до и после
  запуска, применяет migration, проверяет оба SHA-256 checksum, выполняет
  `down`, затем два конкурентных `up`, проверяет итоговую схему и запускает DB
  tests.
- `internal/testkit` разрешает destructive reset только когда одновременно
  заданы `TEST_DATABASE_RESET_ALLOWED=true` и имя БД заканчивается на `_test`.
- GitHub Actions получил отдельный PostgreSQL integration job; сборка runtime
  images зависит и от unit/race, и от integration gate.

## Проверки, выполненные локально через Docker

- `make config` — PASS.
- `docker-compose -f docker-compose.test.yml config --quiet` — PASS.
- `make integration-test` — PASS:
  - clean PostgreSQL 17;
  - migration `up -> down -> concurrent up`;
  - полные 32-byte up/down checksums;
  - ровно одна schema version после конкурентных migrators;
  - jobs/webhook DB tests с `-race` — PASS;
  - test containers и volume отсутствуют после завершения.
- `make test` — PASS: runtime builds, format check, vet и
  `go test -race -count=1 ./...`.

## Fresh Compose smoke

Старый development volume был удалён, основной stack полностью пересобран на
новом volume. API, Worker и PostgreSQL стали healthy; `/live` и `/ready` обоих
Go services вернули `200`.

Schema assertions:

- migration metadata содержит 32-byte up/down checksums;
- FK `inbox_events -> webhook_deliveries` имеет `ON DELETE RESTRICT`;
- уникального индекса на `webhook_deliveries.request_id` нет.

Webhook status matrix на synthetic tenant/data:

| Сценарий | HTTP |
| --- | ---: |
| неправильный content type | 415 |
| неизвестный webhook key | 404 |
| несовпадающий account | 404 |
| отсутствующий account | 204, delivery сохранён как invalid |
| валидная доставка | 204 |
| повтор с тем же request ID и payload | 204 |
| body больше лимита | 413 |

После завершения Worker:

- deliveries: 3 (`parsed=2`, `invalid=1`);
- normalized inbox events: 2, оба `processed`;
- jobs: 4, все `completed`;
- audit effects: 2;
- повторный payload не создал повторный event/effect.

После smoke основной stack и volume с synthetic данными удалены через
`make destroy`; test stack также не оставил containers/volumes.

## GitHub checkpoint policy

После push дождаться фактического PASS GitHub Actions на draft PR `#19`.
Только после этого закрыть `#16`, `#17`, `#18` и `#20`, приложив commit SHA,
команды и результаты. Следующим functional slice остаются OpenAPI и
недостающие OAuth/reconcile/idempotency integration tests.

## Сохраняющиеся hardening-задачи

Issue `#21` остаётся открытым: configurable/observable rate limits, reaper load
test, timestamp semantics, ограничение `migrate down`, management endpoints и
retention/dedup tombstones. Redis не добавлять без измерений и отдельного ADR.

## Follow-up: OpenAPI contract

Issue `#23` добавляет машинно-проверяемый OpenAPI 3.1 contract для всех девяти
routes `cmd/api`: system, OAuth, durable webhook и widget API. Единый
`internal/apicontract` inventory используется и router registration, и
contract test, поэтому изменение path/method требует синхронного обновления
спецификации.

- Contract: `api/openapi.yaml`.
- Semantic/ref/route validation: `make openapi-check` — PASS в Docker.
- Full `make test` после router refactor и новой зависимости — PASS.
- CI получил отдельный обязательный `OpenAPI contract` job.

После push этого follow-up дождаться CI на PR `#22`, приложить evidence к
`#23` и оставить Issue связанным с PR до merge. Следующий functional slice —
OAuth callback/concurrent refresh и webhook reconciliation contract tests.
