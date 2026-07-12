# Project context

Этот файл хранит устойчивые факты и ограничения проекта, но не backlog/status. Каноническое состояние находится в GitHub Issues #12 и phase/atomic Issues. Самый свежий evidence/handoff: [`CHECKPOINT-2026-07-12-job-reaper-batch.md`](CHECKPOINT-2026-07-12-job-reaper-batch.md).

## Snapshot

- Дата: 2026-07-11 (Europe/Moscow).
- Ветка на момент снимка: `codex/workflow-source-state-fence`.
- Базовый commit текущего среза: `cd0d73e` (merge PR `#42`).
- Go module: `github.com/sk1fy/amocrm-pro`.
- Runtime: Go 1.25, PostgreSQL 17 Alpine.
- Redis: не используется и не входит в текущий runtime.
- Стадия: foundation, OAuth/client, webhook reconciliation, atomic widget
  admission, strict CORS, cleanup и widget/webhook workflows с durable effect
  correlation реализованы; P7/P8 hardening продолжается.

При расхождении этого снимка с кодом источником факта являются текущие исходники, миграции и Compose-конфигурация; решение о намерениях уточняется по ADR; план — по `ROADMAP.md`.

## Цель основы

Создать надёжный backend-контур amoCRM, который позже можно встроить в общую Go-микросервисную архитектуру. Границы первого контура:

- публичный `amocrm-api` для callback/widget/webhook endpoints;
- внутренний `amocrm-worker` для amoCRM API, token refresh, webhook reconciliation, sync и workflow;
- PostgreSQL как system of record, durable inbox и очередь jobs;
- один Go module с общими внутренними пакетами и раздельными runtime-процессами;
- одинаковый Docker-based workflow локально и в CI.

## Что присутствует в текущей ветке

### Foundation

- multi-stage Dockerfile с целями API, worker, migrate и test;
- Compose-сервисы `postgres`, `migrate`, `api`, `worker`;
- конфигурация из environment, structured JSON logging и signal-aware shutdown;
- публичный API `/live`, отдельный API management listener с `/live`, `/ready`
  и `/metrics`, внутренний health/metrics listener worker;
- HTTP request ID, recovery и access log middleware;
- Make targets для Docker-only build/test/format/vet/migrations;
- начальный GitHub Actions workflow, собирающий Docker targets.

### SQL schema

Начальная up/down migration описывает:

- `integrations` и `installations`;
- `oauth_states` и `oauth_credentials`;
- `webhook_deliveries` и `inbox_events`;
- `jobs` и `job_attempts`;
- `used_widget_tokens` и `idempotency_keys`;
- `audit_log`;
- ограничения, индексы и `updated_at` triggers.

Схема резервирует модели для OAuth и виджета, но наличие таблиц не означает наличие соответствующих application flows.

### PostgreSQL jobs

- enqueue и получение job по installation;
- конкурентный claim готовых jobs через row locking/lease;
- heartbeat для продления lease;
- bounded retry/backoff и разделение retryable/permanent ошибок;
- статусы completed/retry/failed/dead и запись `job_attempts`;
- worker concurrency и dispatch по типу job.

### OAuth и amoCRM client

- одноразовый OAuth state и атомарное сохранение installation, credentials,
  reconcile job и audit;
- envelope encryption для client secret и OAuth credentials;
- version-fenced token refresh, single retry после `401` и безопасный переход в
  `reauth_required`;
- typed error handling для `429`, validation и transient failures;
- webhook reconciliation с tenant/account validation и сохранением безопасного
  статуса ошибки.

### Widget action admission

- strict disposable JWT может проверяться отдельно от consumption только для
  последующей атомарной action-транзакции;
- `POST /api/v1/widget/actions/ping` требует bounded `Idempotency-Key` и пустое
  body, не хранит raw key;
- active installation lock, jti consumption, idempotency outcome и job enqueue
  выполняются одной PostgreSQL-транзакцией;
- retry с новым JWT и тем же key получает исходный `202/job_id`, mismatch actor
  получает `409`, повтор jti — `401`;
- job status ограничен installation, widget action type и verified user.

### Widget execution hardening и lead status workflow

- durable actor/resource identity хранится в typed job columns, а не извлекается
  из payload JSON;
- idempotency state consistency закреплена migration constraint, stale
  same-request `processing` row имеет bounded recovery;
- maximum disposable JWT lifetime по умолчанию ограничен 15 минутами;
- widget job status actor-scoped и возвращает только action-specific result DTO;
- `workflow.lead.set_status` допускается атомарно и выполняется только для active
  amoCRM admin;
- worker делает GET/compare/PATCH, повторно проверяет admin/tenant/current lease
  и держит lifecycle row lock вокруг PATCH; retry не повторяет уже применённый
  remote effect;
- audit связывает job/actor/lead и числовое desired state без raw amoCRM response.

### Durable webhook pipeline

- endpoint `POST /hooks/amocrm/v1/{webhookKey}`;
- configurable process-local global/per-installation token buckets, bounded
  inactive installation cache и low-cardinality limiter metrics;
- SHA-256 lookup секретного webhook key;
- проверка media type, ограничения body и `account[id]`;
- атомарная запись raw delivery и `webhook.parse` job;
- сохранение невалидного delivery для аудита;
- parser amoCRM form payload и нормализация событий;
- дедупликация через `(installation_id, deduplication_key)`;
- атомарная запись inbox events и `webhook.process_event` jobs;
- базовый process handler, переводящий event в processed и добавляющий audit record.

### Widget browser, cleanup и webhook-origin workflow

- direct browser topology закреплена tenant-bound CORS только для widget routes;
- active origin проверяется через PostgreSQL, actual request связан с verified JWT issuer;
- worker cleanup удаляет expired widget tokens/idempotency rows bounded batches
  под advisory lock и никогда не сокращает replay/TTL safety window;
- webhook dedup вынесен в retained tombstones, поэтому будущий payload cleanup
  не делает историческую доставку снова actionable;
- terminal webhook delivery/inbox payload удаляется по настраиваемому 30-дневному
  default retention; tombstones/runs/effects/jobs/audit остаются durable;
- cleanup pass pressure, workflow routing и finalized workflow jobs публикуются
  как low-cardinality Prometheus metrics;
- typed `status_lead` rule создаёт unique workflow run и convergent transition job;
- immutable job payload сохраняет source и target status; delayed event при уже
  изменившемся remote source завершается как `source_changed` без effect/PATCH;
- run/audit completion повторно проверяет current attempt/lease/active lifecycle,
  а completed audit receipt позволяет reclaimed job завершиться без повторного
  amoCRM GET/PATCH, даже если installation уже отключена;
- widget и webhook lead status mutations фиксируют outbound intent до PATCH;
  incoming target webhook переводит exact effect в observed и не запускает loop.
- async rule configure сохраняет verified widget user как durable actor, worker
  re-checkит active amoCRM admin и применяет create/update/disable через revision CAS;
- immutable per-job configuration receipt делает retry после DB commit
  идемпотентным и хранит typed redacted rule snapshot.

## Что пока отсутствует

- production-grade pagination и распределённое rate limiting (текущий лимитер
  process-local; Redis намеренно отсутствует);
- удаление/ротация amoCRM webhooks и полный uninstall lifecycle;
- stable JSON errors и полный uninstall/revocation lifecycle (`#32`);
- дополнительные domain workflow/sync handlers и generalized registry;
- dashboards, production SLO/alerts и capacity/load tests;
- production integration contracts с окружающими микросервисами.

## Подтверждённые проверки и границы уверенности

- SQL migration agent применил начальную migration `up`, затем `down`, к PostgreSQL 17.
- В исходниках есть unit tests для job classification/backoff и webhook parsing/deduplication/account ID.
- Найденный Docker build blocker из-за неиспользуемого `net/http` в `cmd/worker` исправлен; текущий файл этот import не содержит.
- `make integration-test` проходит на изолированной PostgreSQL 17: migration
  cycle пяти migrations и race-enabled tests jobs/OAuth/webhook/widget/workflow
  с TLS amoCRM stub, включая API management route isolation, webhook limiter
  isolation/eviction/metrics, payload deletion, retained history, replay suppression,
  source-state mismatch, stale completion fence и crash/reclaim receipt recovery.
- `make test` проходит в Docker: runtime builds, formatting, vet и
  `go test -race -count=1 ./...`.

Наличие CI workflow или тестового файла само по себе не считается свидетельством успешного прогона. Новые checkpoint должны перечислять точные выполненные команды и их результат.

## Активные ограничения

- Все Go-команды, сборка, тесты, миграции и PostgreSQL выполняются через Docker/Make; host Go/PostgreSQL не являются частью workflow.
- Redis не добавлять без нового ADR и наблюдаемой необходимости.
- Webhook ingress подтверждает доставку только после durable commit в PostgreSQL.
- Секреты и PII не должны попадать в GitHub Issues, project memory, логи или fixtures.
- Изменения должны сохранять возможность независимого запуска/масштабирования API и worker.

## Каноническая навигация

Агрегированный status и current focus: GitHub Issue #12. Канонический phase
mapping и post-merge protocol: [`ROADMAP.md`](ROADMAP.md). Checkpoints содержат
только evidence/handoff и ведут ссылкой на текущий Issue.

## Правила обновления памяти

При значимом изменении:

1. обновить соответствующий P0–P8 section в `ROADMAP.md`;
2. добавить/обновить запись в `BUGS.md`, если найден дефект или блокер;
3. создать ADR, если меняется архитектурное решение или ограничение;
4. обновить этот snapshot только фактами из кода и фактически выполненных проверок;
5. после восстановления GitHub write access синхронизировать записи в Issues/Project и сохранить URL + commit SHA.

Во всех документах использовать только очищенные данные: никаких secrets, tokens, webhook payloads или PII.
