# Roadmap и структура GitHub Issues

Этот документ сохраняет первоначальную декомпозицию как recovery-copy. Канонический roadmap уже создан в GitHub Issues: `#3` P0 foundation, `#4` P1 OAuth, `#5` P2 amoCRM client, `#9` P3 subscriptions, `#7` P4 ingress, `#6` P5 jobs, `#8` P6 widget, `#10` P7 workflows, `#11` P8 production; umbrella — `#12`. Актуальный handoff находится в [`CHECKPOINT-2026-07-10.md`](CHECKPOINT-2026-07-10.md). Нумерация последующих разделов ниже отражает ранний локальный draft и не должна переопределять GitHub Issues.

## Статусы на 2026-07-10

| Phase | Состояние | Кратко |
| --- | --- | --- |
| P0 | In progress | Foundation присутствует, полная Compose validation продолжается |
| P1 | In progress | SQL schema и up/down есть; up/down применены к PG17, acceptance ещё не закрыт |
| P2 | In progress | PostgreSQL jobs реализованы; нужны integration/regression checks |
| P3 | In progress | Durable webhook pipeline реализован; нужны end-to-end и failure-path checks |
| P4 | Planned | OAuth и application flow installations |
| P5 | Planned | amoCRM client, token refresh, rate limiting и webhook reconciliation |
| P6 | Planned | Widget API, JWT, replay protection и idempotency |
| P7 | Planned | Domain workflows и sync |
| P8 | Planned | Production hardening и интеграция в микросервисный контур |

`In progress` означает наличие реализации, а не завершённый acceptance. Phase можно закрывать только после записи конкретных проверок в checkpoint.

## P0 — Dockerized Go foundation

**GitHub Issue title:** `[P0] Dockerized Go foundation`

**Labels:** `type:epic`, `phase:P0`, `status:in-progress`

**Цель:** создать воспроизводимую основу двух Go runtime-сервисов и мигратора без host Go/PostgreSQL и без Redis.

**Scope/checklist:**

- [x] Один Go module на Go 1.25 с `cmd/api`, `cmd/worker`, `cmd/migrate`.
- [x] Multi-stage Dockerfile для API, worker, migrate и test.
- [x] Compose-стек на PostgreSQL 17 с health/dependency ordering.
- [x] Environment config, structured logging и graceful shutdown.
- [x] `/live`, `/ready`, `/metrics`, request ID, recovery и access log.
- [x] Docker-only Make targets и CI skeleton.
- [ ] Завершить полный `config -> test -> build -> up -> health -> down` прогон.
- [ ] Зафиксировать версии образов, результаты и известные ограничения в checkpoint.

**Acceptance criteria:** API и worker собираются и становятся healthy в Compose; миграции применяются до их старта; остановка не оставляет зависшие процессы; все документированные developer-команды используют Docker/Make.

**Dependencies:** нет.

## P1 — PostgreSQL schema and migrations

**GitHub Issue title:** `[P1] PostgreSQL 17 schema and reversible migrations`

**Labels:** `type:epic`, `phase:P1`, `status:in-progress`, `area:database`

**Цель:** зафиксировать непротиворечивую, reversible и готовую к tenant isolation схему данных.

**Scope/checklist:**

- [x] Таблицы integrations/installations и OAuth storage.
- [x] Durable webhook deliveries/inbox events.
- [x] Jobs/job attempts, widget replay и idempotency storage, audit log.
- [x] Constraints, partial indexes, foreign keys и updated-at triggers.
- [x] Versioned up/down migration и контейнерный migration runner.
- [x] Применить `up`, затем `down`, к PostgreSQL 17 (выполнено migration agent).
- [ ] Добавить repeatable regression: clean `up -> down -> up` и schema assertions.
- [ ] Проверить concurrent migration lock/failure semantics и документировать recovery.
- [ ] Определить retention/cleanup policy для raw deliveries, jobs, tokens и audit.

**Acceptance criteria:** clean PostgreSQL 17 проходит reversible cycle; повторный запуск мигратора безопасен; ключевые constraints и индексы проверены integration tests; retention decision зафиксирован.

**Dependencies:** P0.

## P2 — PostgreSQL durable jobs

**GitHub Issue title:** `[P2] Durable PostgreSQL job queue and worker runtime`

**Labels:** `type:epic`, `phase:P2`, `status:in-progress`, `area:worker`

**Цель:** обеспечить at-least-once выполнение фоновых операций с контролируемыми retry и без Redis.

**Scope/checklist:**

- [x] Enqueue, priority/run-after и installation scope.
- [x] Конкурентный claim с lease и ограниченным batch.
- [x] Worker concurrency, handler registry, timeout и graceful cancellation.
- [x] Lease heartbeat и защита завершения по worker ID.
- [x] Retryable/permanent classification, bounded backoff и dead state.
- [x] История попыток в `job_attempts`.
- [x] Unit tests для classification/backoff присутствуют.
- [ ] Integration tests: concurrent claim, duplicate avoidance и expired lease recovery.
- [ ] Integration tests: complete/fail race, retry exhaustion и unknown handler.
- [ ] Метрики queue depth, oldest ready job, duration, retry/dead counts.
- [ ] Cleanup/requeue operational procedure и runbook.

**Acceptance criteria:** несколько workers безопасно делят очередь; падение worker не теряет job; повторная доставка учитывается handler idempotency; retry/dead поведение и метрики проверены на PostgreSQL 17.

**Dependencies:** P0, P1.

## P3 — Durable webhook inbox

**GitHub Issue title:** `[P3] Durable amoCRM webhook ingress and inbox pipeline`

**Labels:** `type:epic`, `phase:P3`, `status:in-progress`, `area:webhook`

**Цель:** быстро подтверждать webhook только после durable commit, нормализовать события асинхронно и безопасно переживать дубликаты/сбои.

**Scope/checklist:**

- [x] Secret path hash lookup и tenant/account check.
- [x] Только `application/x-www-form-urlencoded`, bounded raw body.
- [x] Транзакционная запись delivery + `webhook.parse` job до `204`.
- [x] `503` при невозможности durable save; аудит invalid delivery.
- [x] Parser, normalized events и stable deduplication key.
- [x] Транзакционная запись inbox events + processing jobs.
- [x] Базовый process handler и audit record.
- [x] Parser/deduplication/account ID unit tests присутствуют.
- [ ] Integration tests: DB failure before commit, retry и отсутствие ложного `204`.
- [ ] Integration tests: duplicate events/deliveries и параллельный parsing.
- [ ] Проверить body/content-type/account mismatch/status matrix.
- [ ] Добавить ingress/parse/process latency и error metrics.
- [ ] Зафиксировать raw payload retention и PII redaction policy.

**Acceptance criteria:** webhook не теряется после `204`; повторная доставка не создаёт повторного business effect; invalid payload наблюдаем; failure paths воспроизводимо проверены.

**Dependencies:** P0, P1, P2.

## P4 — OAuth and installations lifecycle

**GitHub Issue title:** `[P4] OAuth and amoCRM installation lifecycle`

**Labels:** `type:epic`, `phase:P4`, `status:planned`, `area:oauth`

**Цель:** реализовать безопасное подключение, переподключение и отключение tenant installation.

**Scope/checklist:**

- [ ] Bootstrap/management интеграционных client settings без plaintext secrets в БД.
- [ ] Envelope encryption boundary и key-version rotation для client secret/tokens/webhook key.
- [ ] OAuth start: random state, hash-at-rest, TTL и allowlisted return URL.
- [ ] OAuth callback: atomic one-time state consumption и code exchange.
- [ ] Upsert installation/credentials и строгая state machine статусов.
- [ ] Reauthorization, uninstall/disable и credential revocation semantics.
- [ ] Tenant isolation и audit events для lifecycle changes.
- [ ] Unit/integration tests для expiry, replay, mismatch и concurrent callbacks.

**Acceptance criteria:** state нельзя использовать повторно; токены/секреты не сохраняются и не логируются в открытом виде; installation lifecycle детерминирован и tenant-scoped; ошибки callback безопасны для повтора.

**Dependencies:** P1, P2.

## P5 — amoCRM API client and webhook reconciliation

**GitHub Issue title:** `[P5] Resilient amoCRM client, token refresh and webhook reconciliation`

**Labels:** `type:epic`, `phase:P5`, `status:planned`, `area:amocrm-client`

**Цель:** централизовать все исходящие вызовы amoCRM с корректными auth, retry и rate-limit semantics без Redis.

**Scope/checklist:**

- [ ] Typed HTTP client с deadlines, безопасным account-domain validation и bounded responses.
- [ ] Token injection и согласованный refresh с optimistic locking/token version.
- [ ] Single retry после auth refresh; terminal `reauth_required` для неустранимых ошибок.
- [ ] Обработка `429`/`Retry-After`, transient 5xx и jittered backoff.
- [ ] PostgreSQL/process-local rate limiting для текущего масштаба; Redis не добавлять.
- [ ] Pagination и typed error taxonomy.
- [ ] Register/list/delete/reconcile Webhooks через worker jobs.
- [ ] Ротация webhook key и безопасное хранение ciphertext + hash.
- [ ] Contract tests с fake amoCRM server для refresh, 401, 429 и malformed responses.

**Acceptance criteria:** конкурентные workers не повреждают refresh token; retry не создаёт неконтролируемых дублей; rate limits соблюдаются; desired webhook settings сходятся к фактическим.

**Dependencies:** P2, P4.

## P6 — Widget API, JWT and idempotency

**GitHub Issue title:** `[P6] Secure widget API, one-time JWT and idempotency`

**Labels:** `type:epic`, `phase:P6`, `status:planned`, `area:widget-api`

**Цель:** дать amoCRM widget tenant-safe API с replay protection и асинхронными операциями.

**Scope/checklist:**

- [ ] Валидация JWT signature/algorithm, issuer, audience, expiry и required claims.
- [ ] Atomic one-time `jti` claim в PostgreSQL и cleanup expired tokens.
- [ ] Связка account/user/integration claims с active installation.
- [ ] CORS/origin policy, body limits, request ID и стабильный error envelope.
- [ ] Idempotency-key lifecycle и request hash mismatch protection.
- [ ] Endpoints создания jobs и tenant-scoped чтения статуса/result.
- [ ] Authorization matrix и негативные/replay/concurrency tests.

**Acceptance criteria:** повторный JWT отклоняется атомарно; пользователь не видит чужую installation/job; повтор идентичного запроса безопасен; API не возвращает secrets или raw PII.

**Dependencies:** P2, P4, P5.

## P7 — Domain workflows and synchronization

**GitHub Issue title:** `[P7] Idempotent domain workflows and amoCRM synchronization`

**Labels:** `type:epic`, `phase:P7`, `status:planned`, `area:domain`

**Цель:** заменить инфраструктурный webhook process handler прикладными, наблюдаемыми workflow.

**Scope/checklist:**

- [ ] Зафиксировать поддерживаемые entity/event types и versioned normalized contract.
- [ ] Реализовать idempotent handlers и явные permanent/retryable outcomes.
- [ ] Out-of-order/stale event policy и reconciliation jobs.
- [ ] Bulk/paginated sync с checkpoint/cursor и controlled fan-out.
- [ ] Workflow result/audit contract и ручной replay без повторного side effect.
- [ ] Контракты публикации/вызова окружающих микросервисов.
- [ ] Integration/contract tests основных business scenarios.

**Acceptance criteria:** повтор, перестановка и частичный сбой не нарушают domain invariants; sync возобновляется с checkpoint; внешние side effects идемпотентны и трассируемы.

**Dependencies:** P3, P5, P6; конкретные workflow могут зависеть только от нужного подмножества.

## P8 — Production hardening and microservice integration

**GitHub Issue title:** `[P8] Production hardening and microservice integration readiness`

**Labels:** `type:epic`, `phase:P8`, `status:planned`, `area:operations`

**Цель:** подготовить сервисы к безопасной эксплуатации и подключению к общей платформе.

**Scope/checklist:**

- [ ] SLI/SLO, dashboards и alerts для HTTP, DB pool, queue, webhook и amoCRM API.
- [ ] Trace/correlation propagation и redaction policy для logs/metrics/traces.
- [ ] Migration, deploy, rollback, backup/restore и disaster recovery runbooks.
- [ ] Retention/cleanup jobs и capacity/load tests PostgreSQL queue/inbox.
- [ ] Security review: secrets, SSRF/domain validation, dependency/container scanning, least privilege.
- [ ] Failure tests: DB restart, worker crash, amoCRM timeout/429 и graceful deploy.
- [ ] Platform contracts: configuration/secrets, service discovery, ingress, observability и ownership.
- [ ] Решение о необходимости Redis только по измерениям и через отдельный ADR.

**Acceptance criteria:** согласованы production contracts и ownership; SLO наблюдаемы; restore/rollback/failure procedures проверены; security findings обработаны; сервис имеет release/runbook checklist.

**Dependencies:** P0–P7 по мере включения соответствующих возможностей.

## Checkpoint template

Шаблон предназначен для комментария в соответствующем GitHub Issue и параллельного versioned обновления этих файлов. Не вставлять secrets, tokens, webhook bodies или PII.

```md
## Checkpoint — YYYY-MM-DD HH:MM TZ

- Phase / Issue: P? / #?
- Branch: `...`
- Commit / worktree base: `...`
- Owner / agent: `...`
- Goal of this checkpoint: ...

### Completed

- [ ] ...

### Changed

- `path/to/file`: краткое описание без secrets/PII

### Validation actually run

| Command/check | Environment | Result | Evidence/notes |
| --- | --- | --- | --- |
| `make ...` | Docker / image version | pass/fail/not run | sanitized summary |

### Decisions

- ADR/link or `none`.

### Bugs and blockers

- BUG-ID / GitHub Issue link / `none`.

### Risks and assumptions

- ...

### Next actions

1. ...

### Memory sync

- [ ] `CONTEXT.md` updated if the factual snapshot changed.
- [ ] `ROADMAP.md` checklist/status updated.
- [ ] `BUGS.md` updated for defects/blockers.
- [ ] ADR added/updated for architectural decisions.
- [ ] GitHub Issue/Project updated (or connector blocker recorded).
- Local commit SHA: `...`
- GitHub URLs: `...`
```

## Future GitHub synchronization

Когда write access будет восстановлен:

1. создать Issues с точными заголовками P0–P8 из этого файла;
2. создать labels `type:epic`, `phase:P0`…`phase:P8`, status и area labels;
3. добавить Issues в Project с полями `Phase`, `Status`, `Owner`, `Risk`, `Last checkpoint`;
4. перенести checklist и последний checkpoint, не копируя secrets/PII;
5. создать отдельный blocker Issue из `BUG-001` либо закрыть его ссылкой на исправление прав;
6. записать URL Issues/Project и commit SHA обратно в project-memory;
7. в дальнейшем обновлять GitHub и versioned fallback в одном checkpoint, пока не принято отдельное решение отказаться от локальной копии.
