# amoCRM Go backend

Docker-first backend для интеграций и JS-виджетов amoCRM. Проект состоит из
двух независимо запускаемых Go-процессов, мигратора и PostgreSQL. Redis в
текущей архитектуре не используется.

## Состояние проекта

На `main` реализован функциональный vertical slice:

- OAuth start/callback, зашифрованные credentials и version-fenced refresh;
- специализированный HTTP-клиент amoCRM API v4;
- управление и reconciliation webhook-подписок;
- durable webhook ingress, parsing, deduplication и PostgreSQL jobs;
- одноразовая аутентификация виджета, Idempotency-Key и actor-scoped job status;
- асинхронное изменение статуса сделки с повторной проверкой прав администратора;
- webhook-origin lead-status workflow, loop prevention и durable effect history;
- bounded cleanup, backlog metrics и expired-lease reaping;
- capacity-проверка reaper на 100 000 просроченных jobs.

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
internal/integration/     amoCRM OAuth/API client
internal/jobs/            PostgreSQL queue и worker runtime
internal/maintenance/     bounded cleanup scheduler
internal/oauth/           OAuth application layer и token provider
internal/webhook/         webhook ingress, parser и workflows
internal/widgetapi/       widget actions и job handlers
internal/widgetauth/      disposable JWT validation
internal/widgetcors/      tenant-bound CORS
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
