# amoCRM Go backend

Docker-first backend для интеграций и JS-виджетов amoCRM. Базовый режим состоит из
двух независимо запускаемых Go-процессов, мигратора, operator CLI и PostgreSQL. Redis в
текущей архитектуре не используется.

Activity v0 подключается явно, с отдельными логическими БД Activity и CRM Events,
собственным сборщиком/очередью, durable Core outbox и защищёнными embedded/gRPC
адаптерами. Отдельные бинарники — `cmd/activity` и `cmd/crm-events`. Инструкции:
[Activity runbook](docs/runbooks/activity-v0.md),
[владельцы и границы](docs/adr/0010-activity-v0-service-ownership.md),
[фактические проверки и ограничения](docs/verification/activity-v0-results.md).
`make activity-up` запускает изолированный development-стек; обычный стек сохраняет
`ACTIVITY_MODE=off` и прежние контракты lead-status.

## Состояние проекта

Реализован функциональный vertical slice:

- OAuth start/callback, зашифрованные credentials и version-fenced refresh;
- audited operator CLI для нескольких integrations и capability guard в API/worker;
- специализированный HTTP-клиент amoCRM API v4;
- управление и reconciliation webhook-подписок;
- durable webhook ingress, parsing, deduplication и PostgreSQL jobs;
- одноразовая аутентификация виджета, Idempotency-Key и actor-scoped job status;
- асинхронное изменение статуса сделки с повторной проверкой прав администратора;
- webhook-origin lead-status workflow, loop prevention и durable effect history;
- bounded cleanup, backlog metrics и expired-lease reaping;
- capacity-проверка reaper на 100 000 просроченных jobs;
- lead-status как отдельный модуль с совместимостью прежних URL и jobs;
- лимиты widget API, fair claiming и общий лимит leases по integration;
- метрики backlog, ожидания и исполнения по ограниченному каталогу сервисов.

Последний CI на текущем `main` прошёл успешно. Функциональный MVP существует,
но production hardening и полный lifecycle интеграции ещё не завершены.
Канонический backlog находится в
[#12](https://github.com/sk1fy/amocrm-pro/issues/12) и phase Issues.

### Проверка реального JS-виджета

Widget API принимает основной Web SDK header `X-Auth-Token` и совместимый
`Authorization: Bearer`; одновременная подача обоих заголовков отклоняется.
`X-Auth-Token` включён в tenant-bound CORS contract. Автоматизированный контракт
покрыт unit и PostgreSQL integration tests, но реальный browser E2E из
установленного приватного виджета ещё не выполнен и отслеживается в
[#55](https://github.com/sk1fy/amocrm-pro/issues/55).

Подробности и preconditions: [private-widget-e2e.md](docs/runbooks/private-widget-e2e.md).

## Архитектура

```text
JS-виджет / amoCRM OAuth / amoCRM Webhooks
                    |
                    v
               amocrm-api
                    |
                    v
               PostgreSQL
                    |
                    v
              amocrm-worker
                    |
                    v
              amoCRM API v4
```

API выполняет только bounded HTTP admission и durable commit. Внешние amoCRM
API calls, workflow, retries, token refresh и cleanup выполняет worker.

Подробное фактическое описание: [docs/architecture.md](docs/architecture.md).
Принятые решения: [docs/adr/](docs/adr/).

## Сервисы

| Компонент | Назначение | Порт по умолчанию |
| --- | --- | --- |
| `api` | OAuth, widget API и webhook ingress | `127.0.0.1:8080` |
| `api` management | Liveness, readiness и Prometheus metrics | `127.0.0.1:8082` |
| `worker` | Jobs, amoCRM API, workflow и cleanup | `127.0.0.1:8081` |
| `migrate` | Применение SQL-миграций | нет |
| `integrations` | Operator CLI: provisioning, secrets, disable и service grants | нет |
| `postgres` | System of record, inbox и очередь | `127.0.0.1:5432` |

Runtime: Go 1.25 и PostgreSQL 17 Alpine.

## Запуск

Нужны Docker с Docker Compose и `make`. Host Go/PostgreSQL не являются
поддерживаемым workflow.

```sh
make config
make build
make up
make ps
```

Полезные команды:

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

`make destroy` останавливает development-стек и удаляет его PostgreSQL volume.
Полный rollback схемы защищён отдельным подтверждением и описан в
[migrate-down.md](docs/runbooks/migrate-down.md).

Подключение дополнительных виджетов, ротация secrets и отключение описаны в
[integrations.md](docs/runbooks/integrations.md). Env bootstrap создаёт только
первоначальную integration и не перезаписывает изменения оператора. Новые
integrations требуют явного выбора services; lead-status проверяется в API
до расходования JWT/idempotency и повторно в worker перед изменением.

Лимиты, порядок обновления всех workers и нагрузочные проверки описаны в
[widget-capacity.md](docs/runbooks/widget-capacity.md). Для нового worker нужна
миграция 000009; все replicas должны использовать одинаковый
`WORKER_INTEGRATION_CONCURRENCY`.

## Публичные endpoints

OAuth:

- `GET /oauth/amocrm/start`;
- `GET /oauth/amocrm/callback`.

Webhooks:

- `POST /hooks/amocrm/v1/{webhookKey}`.

Widget API:

- `GET /api/v1/widget/bootstrap`;
- `POST /api/v1/widget/actions/ping`;
- `POST /api/v1/widget/actions/leads/set-status`;
- `POST /api/v1/widget/workflow-rules/lead-status/configure`;
- `GET /api/v1/widget/jobs/{jobID}`.

Полный контракт: [api/openapi.yaml](api/openapi.yaml). Management endpoints
`/live`, `/ready` и `/metrics` изолированы на management listener и не входят в
публичную OpenAPI-схему.

## Структура

```text
cmd/api/                  публичный HTTP-сервис
cmd/worker/               обработчик фоновых jobs
cmd/migrate/              контейнерный мигратор
cmd/integrations/         operator CLI
internal/integration/     amoCRM OAuth/API client
internal/integrations/    audited provisioning
internal/services/        каталог, capability и event contracts
internal/services/leadstatus/  продуктовый модуль lead-status
internal/jobs/            PostgreSQL queue и worker runtime
internal/maintenance/     bounded cleanup scheduler
internal/oauth/           OAuth application layer и token provider
internal/webhook/         webhook ingress, parser и workflows
internal/widgetapi/       общий admission, execution guards и polling
internal/widgetauth/      disposable JWT validation
internal/widgetcors/      tenant-bound CORS
internal/widgetlimit/     лимиты API по verified tenant
migrations/               versioned up/down migrations
docs/                     активная документация
```

## Документация

- [Documentation index](docs/README.md)
- [Architecture](docs/architecture.md)
- [Project context](docs/project-memory/CONTEXT.md)
- [Recovery index](docs/project-memory/ROADMAP.md)
- [Known bugs](docs/project-memory/BUGS.md)
- [ADRs](docs/adr/)
- [Runbooks](docs/runbooks/)
- [Historical archive](docs/archive/)
- [Audits](docs/audits/)

Исторические checkpoints не являются текущим backlog или resume order.

## Безопасность

Не публикуйте client secrets, OAuth access/refresh tokens, webhook keys, ключи
шифрования, `.env` или персональные данные из webhook payloads. Для examples и
tests используйте только вымышленные значения. Production deployment должен
использовать собственный случайный encryption key, TLS, secret manager/KMS,
ограниченный management listener и отдельную политику backup/restore.
