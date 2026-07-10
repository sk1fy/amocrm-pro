# amoCRM Go backend

Основа backend-интеграции amoCRM для последующего включения в Go-микросервисную архитектуру. В репозитории находятся два независимо запускаемых Go-сервиса, служебный мигратор и PostgreSQL. Redis в текущей архитектуре не используется.

Исходный архитектурный план: [`amocrm-go-backend-architecture.md`](amocrm-go-backend-architecture.md). Фактическое состояние реализации и принятые решения фиксируются в [`docs/project-memory/`](docs/project-memory/) и [`docs/adr/`](docs/adr/).

## Текущее состояние

Снимок на 2026-07-10:

- Docker-образы, Compose-стек, конфигурация, structured logging, graceful shutdown, health endpoints, Prometheus endpoint и CI-скелет присутствуют;
- начальная SQL-схема и checksum-aware reversible мигратор присутствуют;
- PostgreSQL-backed очередь jobs реализует lease/heartbeat, attempt fencing, retry/backoff/dead state, panic recovery и атомарные domain failure observers;
- durable webhook pipeline принимает bounded `application/x-www-form-urlencoded`, проверяет secret URL и `account[id]`, транзакционно сохраняет delivery + job, нормализует и дедуплицирует inbox events;
- реализованы OAuth start/callback, encrypted credentials, coordinated token refresh, amoCRM API client, webhook reconciliation и базовый API виджета с одноразовым JWT;
- прикладные domain workflows, полный idempotency contract виджета и production hardening остаются следующими этапами.

Docker build, vet и race-enabled unit tests проходят. Актуальный validation gap и точный порядок возобновления зафиксированы в [checkpoint 2026-07-10](docs/project-memory/CHECKPOINT-2026-07-10.md): новые PostgreSQL integration tests ещё нужно запустить на чистом изолированном volume после последних изменений initial migration.

## Сервисы

| Компонент | Назначение | Порт по умолчанию |
| --- | --- | --- |
| `api` | Webhook ingress и публичные HTTP endpoints | `127.0.0.1:8080` |
| `worker` | Выполнение PostgreSQL jobs и внутренний health server | `127.0.0.1:8081` |
| `migrate` | Применение SQL-миграций перед запуском сервисов | нет |
| `postgres` | Основное хранилище, inbox и очередь jobs | `127.0.0.1:5432` |

`api` и `worker` собираются из одного Go-модуля (`github.com/sk1fy/amocrm-pro`) на Go 1.25. PostgreSQL запускается из образа PostgreSQL 17 Alpine. Архитектурные причины описаны в [ADR-0001](docs/adr/0001-postgresql-without-redis.md), [ADR-0002](docs/adr/0002-two-go-binaries-one-module.md) и [ADR-0003](docs/adr/0003-docker-only-runtime-and-tooling.md).

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
make vet
make fmt-check
make tidy
make db-shell
make down
```

`make destroy` останавливает стек и удаляет локальный volume PostgreSQL вместе со всеми данными.

## HTTP endpoints

Оба долгоживущих сервиса публикуют:

- `GET /live` — liveness;
- `GET /ready` — readiness с проверкой PostgreSQL;
- `GET /metrics` — метрики Prometheus.

API также принимает Webhooks:

- `POST /hooks/amocrm/v1/{webhookKey}`.

Webhook endpoint ожидает `Content-Type: application/x-www-form-urlencoded`. Его URL содержит секрет установки; тело ограничено по размеру и сохраняется в PostgreSQL до ответа `204`. Если delivery нельзя надёжно сохранить, API отвечает `503`, чтобы источник мог повторить доставку.

## Структура

```text
cmd/api/                  публичный HTTP-сервис
cmd/worker/               обработчик фоновых jobs
cmd/migrate/              контейнерный мигратор
internal/jobs/            PostgreSQL queue и worker runtime
internal/webhook/         webhook ingress, parser и handlers
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
- [`docs/adr/`](docs/adr/) — принятые архитектурные решения.

## Безопасность

Не публикуйте и не коммитьте client secrets, OAuth access/refresh tokens, webhook keys, ключи шифрования, содержимое `.env` или персональные данные из webhook payloads. Raw Webhooks могут содержать PII: не копируйте их в Issues, pull requests, логи, тестовые fixtures или проектную память без необратимой очистки. Для примеров используйте только вымышленные значения.
