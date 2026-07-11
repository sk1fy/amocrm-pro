# amoCRM Go backend

Основа backend-интеграции amoCRM для последующего включения в Go-микросервисную архитектуру. В репозитории находятся два независимо запускаемых Go-сервиса, служебный мигратор и PostgreSQL. Redis в текущей архитектуре не используется.

Исходный архитектурный план: [`amocrm-go-backend-architecture.md`](amocrm-go-backend-architecture.md). Фактическое состояние реализации и принятые решения фиксируются в [`docs/project-memory/`](docs/project-memory/) и [`docs/adr/`](docs/adr/).

## Текущее состояние

Снимок на 2026-07-11:

- Docker-образы, Compose-стек, конфигурация, structured logging, graceful shutdown, health endpoints, Prometheus endpoint и CI-скелет присутствуют;
- начальная SQL-схема и checksum-aware reversible мигратор присутствуют;
- PostgreSQL-backed очередь jobs реализует lease/heartbeat, attempt fencing, retry/backoff/dead state, panic recovery и атомарные domain failure observers;
- durable webhook pipeline принимает bounded `application/x-www-form-urlencoded`, проверяет secret URL и `account[id]`, транзакционно сохраняет delivery + job, нормализует и дедуплицирует inbox events;
- реализованы OAuth start/callback, encrypted credentials, coordinated token refresh, amoCRM API client, webhook reconciliation и базовый API виджета с одноразовым JWT;
- widget action admission атомарно связывает одноразовый JWT, Idempotency-Key,
  durable actor/resource ownership и PostgreSQL job;
- первый реальный workflow `lead.set_status` повторно проверяет active tenant и
  active admin actor, затем выполняет amoCRM GET/compare/PATCH и возвращает
  только типизированный redacted result;
- browser widget обращается к API напрямую через tenant-bound CORS: preflight
  допускает только HTTPS origin активной installation, actual request связан с JWT issuer;
- worker выполняет bounded cleanup replay/idempotency rows с PostgreSQL advisory lock;
- terminal webhook delivery/inbox payload удаляется после настраиваемого
  30-дневного retention, а tombstones/runs/effects остаются durable;
- typed `status_lead` rules создают webhook-origin workflow runs, а durable
  tombstones и outbound effects предотвращают replay и коррелируют self-effects;
- verified widget admin может асинхронно create/update/disable lead-status rule
  через revision CAS; worker проверяет live admin rights, а durable receipt
  защищает retry после commit;
- worker экспортирует bounded cleanup и workflow Prometheus metrics без
  tenant/resource/error labels.

Docker build, vet, race-enabled unit tests и изолированный PostgreSQL integration
gate проходят. OAuth callback/refresh и webhook reconciliation покрыты
конкурентными contract tests; актуальный handoff находится в
[webhook payload retention checkpoint](docs/project-memory/CHECKPOINT-2026-07-11-webhook-payload-retention-metrics.md).

## Сервисы

| Компонент | Назначение | Порт по умолчанию |
| --- | --- | --- |
| `api` | Webhook ingress и публичные HTTP endpoints | `127.0.0.1:8080` |
| `worker` | Выполнение PostgreSQL jobs, periodic cleanup и внутренний health server | `127.0.0.1:8081` |
| `migrate` | Применение SQL-миграций перед запуском сервисов | нет |
| `postgres` | Основное хранилище, inbox и очередь jobs | `127.0.0.1:5432` |

`api` и `worker` собираются из одного Go-модуля (`github.com/sk1fy/amocrm-pro`) на Go 1.25. PostgreSQL запускается из образа PostgreSQL 17 Alpine. Архитектурные причины описаны в [ADR-0001](docs/adr/0001-postgresql-without-redis.md), [ADR-0002](docs/adr/0002-two-go-binaries-one-module.md), [ADR-0003](docs/adr/0003-docker-only-runtime-and-tooling.md), [ADR-0004](docs/adr/0004-widget-browser-and-cleanup-contract.md), [ADR-0005](docs/adr/0005-webhook-workflow-effect-correlation.md), [ADR-0006](docs/adr/0006-webhook-payload-retention-and-metrics.md) и [ADR-0007](docs/adr/0007-async-rule-management-principal-and-cas.md).

## Запуск и разработка

Для работы нужны Docker с Docker Compose и `make`. Локальная установка Go и PostgreSQL не требуется и не должна использоваться для проектных команд. Значения по умолчанию позволяют запустить development-стек без файла `.env`; доступные настройки перечислены в `.env.example`.

```sh
make config
make build
make up
make ps
```

Полезные команды, все выполняемые через Docker:

```sh
make logs
make migrate
make test
make openapi-check
make integration-test
make vet
make fmt-check
make tidy
make db-shell
make down
```

`make destroy` останавливает стек и удаляет локальный volume PostgreSQL вместе со всеми данными.

`make integration-test` создаёт отдельный Compose project с БД
`amocrm_test`, выполняет migration cycle `up -> down -> concurrent up`,
проверяет checksums/schema и запускает race-enabled PostgreSQL tests пакетов
jobs, OAuth, webhook, widget и amoCRM workflow. Test
containers и volume удаляются автоматически. Защита test helper требует
одновременно явный `TEST_DATABASE_RESET_ALLOWED=true` и имя БД с суффиксом
`_test`, поэтому destructive reset не может случайно использовать обычную
development DB.

Машиночитаемый HTTP-контракт находится в
[`api/openapi.yaml`](api/openapi.yaml). `make openapi-check` семантически
проверяет документ и полный список реализованных routes через Go validator в
Docker; отдельный OpenAPI job является обязательной частью CI.

## HTTP endpoints

Оба долгоживущих сервиса публикуют:

- `GET /live` — liveness;
- `GET /ready` — readiness с проверкой PostgreSQL;
- `GET /metrics` — метрики Prometheus.

API также принимает Webhooks:

- `POST /hooks/amocrm/v1/{webhookKey}`.

Widget API публикует одноразово-аутентифицированные endpoints:

- `GET /api/v1/widget/bootstrap`;
- `POST /api/v1/widget/actions/ping`;
- `POST /api/v1/widget/actions/leads/set-status` — admin-only convergent workflow;
- `POST /api/v1/widget/workflow-rules/lead-status/configure` — async admin-checked revision CAS;
- `GET /api/v1/widget/jobs/{jobID}` — actor-scoped status с typed result.

Webhook endpoint ожидает `Content-Type: application/x-www-form-urlencoded`. Его URL содержит секрет установки; тело ограничено по размеру и сохраняется в PostgreSQL до ответа `204`. Если delivery нельзя надёжно сохранить, API отвечает `503`, чтобы источник мог повторить доставку.

## Структура

```text
cmd/api/                  публичный HTTP-сервис
cmd/worker/               обработчик фоновых jobs
cmd/migrate/              контейнерный мигратор
internal/jobs/            PostgreSQL queue и worker runtime
internal/maintenance/     bounded PostgreSQL cleanup scheduler
internal/webhook/         webhook ingress, parser и handlers
internal/widgetcors/      tenant-bound browser CORS policy
internal/installations/   доступ к данным установок
internal/platform/        config, logging, PostgreSQL, migrations
internal/transport/       HTTP server и middleware
migrations/               versioned up/down SQL
docs/adr/                 архитектурные решения
docs/project-memory/      versioned fallback проектной памяти
```

## Проектная память

GitHub Issues являются основной долговременной памятью проекта (`#2` — контекст, `#12` — umbrella program, `#3`–`#11` — фазы, `#13`–`#18` — ADR/bugs). Versioned recovery-копия хранится в репозитории:

- [`CONTEXT.md`](docs/project-memory/CONTEXT.md) — текущий срез, ограничения и следующие шаги;
- [`ROADMAP.md`](docs/project-memory/ROADMAP.md) — структура GitHub Issues P0–P8 и checkpoint template;
- [`BUGS.md`](docs/project-memory/BUGS.md) — открытые и закрытые дефекты/блокеры;
- [`CHECKPOINT-2026-07-10.md`](docs/project-memory/CHECKPOINT-2026-07-10.md) — точный handoff текущей ветки, validation evidence и resume order;
- [`CHECKPOINT-2026-07-11.md`](docs/project-memory/CHECKPOINT-2026-07-11.md) — repeatable PostgreSQL gate, fresh Compose smoke и актуальный следующий шаг;
- [`CHECKPOINT-2026-07-11-oauth-reconcile.md`](docs/project-memory/CHECKPOINT-2026-07-11-oauth-reconcile.md) — OAuth concurrency, webhook reconciliation, исправленные дефекты и resume order;
- [`CHECKPOINT-2026-07-11-widget-idempotency.md`](docs/project-memory/CHECKPOINT-2026-07-11-widget-idempotency.md) — atomic widget admission, replay/rollback contracts и следующий workflow slice;
- [`CHECKPOINT-2026-07-11-lead-status-workflow.md`](docs/project-memory/CHECKPOINT-2026-07-11-lead-status-workflow.md) — execution hardening, typed ownership/result и первый real amoCRM workflow;
- [`CHECKPOINT-2026-07-11-webhook-correlation-retention.md`](docs/project-memory/CHECKPOINT-2026-07-11-webhook-correlation-retention.md) — strict browser CORS, bounded cleanup, tombstones и webhook-origin effect correlation;
- [`CHECKPOINT-2026-07-11-webhook-payload-retention-metrics.md`](docs/project-memory/CHECKPOINT-2026-07-11-webhook-payload-retention-metrics.md) — finite raw payload retention, durable history и cleanup/workflow metrics;
- [`CHECKPOINT-2026-07-11-rule-management-contract.md`](docs/project-memory/CHECKPOINT-2026-07-11-rule-management-contract.md) — async rule management principal, CAS/receipt и canonical Issue sync;
- [`docs/adr/`](docs/adr/) — принятые архитектурные решения.

## Безопасность

Не публикуйте и не коммитьте client secrets, OAuth access/refresh tokens, webhook keys, ключи шифрования, содержимое `.env` или персональные данные из webhook payloads. Raw Webhooks могут содержать PII: не копируйте их в Issues, pull requests, логи, тестовые fixtures или проектную память без необратимой очистки. Для примеров используйте только вымышленные значения.
