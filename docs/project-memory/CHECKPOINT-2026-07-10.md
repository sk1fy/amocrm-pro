# Checkpoint: backend foundation handoff

Дата: 2026-07-10 (Europe/Moscow)

Ветка: `codex/amocrm-backend-foundation`

База ветки: `edb4a39` (`main`, до реализации)

Этот документ является точкой входа для следующей Codex-сессии. Источник
плана и найденных дефектов — GitHub Issues репозитория `sk1fy/amocrm-pro`;
исходники, миграции и тесты в этой ветке являются источником фактического
состояния реализации.

## Неподвижные ограничения

- Redis пока не использовать.
- Go, PostgreSQL, миграции, форматирование и тесты запускать через Docker.
- Backend проектируется как самостоятельный контур, который позднее войдёт
  в Go-микросервисную архитектуру.
- Не сохранять secrets, OAuth tokens, webhook keys, raw payloads или PII в
  GitHub Issues, документации, fixtures и логах.
- Значимые решения фиксировать ADR; этапы, checkpoints и bugs — в GitHub
  Issues. Этот каталог остаётся versioned recovery-копией памяти.

## GitHub memory index

- `#2` — постоянный context/invariants.
- `#3` — P0 Dockerized foundation.
- `#4` — P1 OAuth/installations lifecycle.
- `#5` — P2 amoCRM client/token refresh/rate limiting.
- `#9` — P3 webhook subscription reconciliation.
- `#7` — P4 durable webhook ingress/inbox.
- `#6` — P5 PostgreSQL jobs/Worker.
- `#8` — P6 widget API/JWT/idempotency.
- `#10` — P7 domain workflows.
- `#11` — P8 production hardening.
- `#12` — umbrella program issue.
- `#13` — ADR backlog.
- `#14` — resolved Worker build regression.
- `#15` — resolved GitHub connector permission incident.
- `#16` — webhook ingress invariants/regressions.
- `#17` — migration runner reversibility/checksums.
- `#18` — Worker attempt fencing/panic/shutdown.

## Реализовано в рабочем дереве

### Runtime и инфраструктура

- Один Go module `github.com/sk1fy/amocrm-pro`, Go 1.25.
- Отдельные binaries `cmd/api`, `cmd/worker`, `cmd/migrate`.
- Multi-stage Dockerfile, Compose с PostgreSQL 17, API, Worker и мигратором;
  Redis отсутствует.
- Docker-only Make targets, GitHub Actions skeleton, env validation,
  structured JSON logs, graceful shutdown, `/live`, `/ready`, `/metrics`.
- API/Worker runtime containers работают от non-root user, read-only FS.

### PostgreSQL и jobs

- Reversible initial schema для integrations/installations, OAuth,
  webhook inbox, jobs/attempts, widget replay/idempotency и audit.
- Собственный migration runner: строгие парные имена, advisory lock,
  SHA-256 обоих направлений, divergence/unknown/mutated migration preflight,
  `up` и `down`.
- PostgreSQL queue с `SKIP LOCKED`, priority/run-after, lease/heartbeat,
  attempt fencing, retry/backoff/dead, attempt history и bounded reaping.
- Handler panic переводится в sanitized retryable failure. Обновление статуса
  job и webhook failure state выполняется одной DB-транзакцией через observer.

### amoCRM integration

- OAuth start/callback: hashed one-time state с TTL, code exchange,
  encrypted credentials, installation upsert и audit.
- AES-256-GCM versioned keyring с domain-specific AAD; development sample key
  запрещён вне `APP_ENV=development`.
- Bootstrap integration configuration из environment без plaintext secret в
  БД.
- Typed amoCRM HTTP client: account-domain allowlist, bounded responses,
  token injection, coordinated refresh, один повтор после 401, process-local
  per-integration/per-account rate limits, webhook list/register/delete.
- Worker job для convergence desired webhook subscriptions.

### Durable webhook pipeline

- Secret-path hash lookup, content-type/body/account validation, global и
  per-installation ingress limiters, один bounded request deadline.
- Delivery и parse job сохраняются атомарно до ответа `204`; failure даёт
  retryable HTTP response. Request ID используется только для correlation.
- Receipt-time snapshot allowed events, bounded parser amoCRM form payload,
  stable event deduplication, tenant-scoped storage и async processing.
- Inbox FK не позволяет удалить delivery вместе с dedup record; retention
  должен redaction-обновлять payload, а не каскадно удалять dedup rows.

### Widget foundation

- Строгая HS256 disposable JWT validation: issuer/audience/time/claims,
  installation-bound encrypted secret и атомарная replay-защита `jti` в PG.
- Tenant-scoped bootstrap, proof async `ping` action и чтение job status.

## Проверки, реально выполненные перед checkpoint

- `make fmt` — PASS, Go форматировался в `golang:1.25-alpine`.
- `make test` — PASS после всех последних изменений: Docker builds для
  migrate/API/Worker, `gofmt -l`, `go vet ./...`,
  `go test -race -count=1 ./...`.
- Отдельная ручная Docker-проверка на изолированной свежей PostgreSQL 17:
  current migration + `TEST_DATABASE_URL` +
  `go test -race -count=1 -v ./internal/jobs ./internal/webhook` — PASS, все
  8 tests, включая 3 jobs DB scenarios и webhook
  snapshot/request-id/tenant/FK scenario. Repeatable Make/CI gate для этого
  прогона ещё не добавлен.
- Предыдущий Compose smoke (до последних усилений migration/jobs): API и
  Worker healthy, webhook `204`, parse/process jobs completed, повторный
  payload не создал второй business effect.
- Предыдущий PostgreSQL 17 migration smoke: `up -> down -> up` PASS.

Важно: initial migration была усилена после предыдущего Compose smoke.
Старый локальный Docker volume содержит дорефакторинговую схему и не является
валидным acceptance evidence. Следующая сессия должна начать DB-проверки на
новом изолированном volume.

## Следующие действия в порядке приоритета

1. На чистом Compose project/volume выполнить migration `up -> down -> up`,
   проверить записанные checksums и schema constraints.
2. Превратить уже прошедший ручной DB integration прогон в Docker-only
   Compose/Make/CI gate; затем повторить end-to-end webhook smoke на свежем
   volume.
3. Закрыть GitHub bugs `#16`, `#17`, `#18` только после комментария с
   конкретными командами и PASS evidence.
4. Не запускать test reset против development DB: integration target должен
   всегда использовать отдельный Compose project/volume и удалять его после
   прогона.
5. Добавить `api/openapi.yaml` для уже существующих routes и синхронизировать
   README с контрактом API.
6. Дополнить tests для concurrent OAuth refresh/callback, reconcile error
   paths и production bootstrap.
7. Продолжить P6: настоящий idempotency-key contract и прикладные widget
   actions; затем P7 business workflows.
8. P2 performance follow-up: нагрузочно проверить bounded job reaper. Сейчас
   до 100 jobs обрабатываются несколькими SQL на row внутри claim transaction;
   correctness подтверждена, но на loaded DB возможен timeout/starvation.

## Команды возобновления

Все Go/DB действия выполняются через Docker:

```sh
git switch codex/amocrm-backend-foundation
make config
make test

# Перед свежим локальным acceptance удалить только development volume этого
# Compose project. Команда уничтожает локальные DB-данные.
make destroy
make up
make ps
```

Перед изменением initial migration убедиться, что она ещё не была выпущена.
После merge/release `000001_init.*.sql` считается immutable; любые изменения
схемы выполняются новой парой migration files.

## Критерий безопасного продолжения

Следующая сессия сначала читает этот checkpoint, `#2`, `#12` и issue нужной
фазы. После каждого meaningful slice она добавляет sanitized Issue comment с
branch/commit, реально выполненными проверками, открытыми рисками и следующим
шагом, а затем обновляет этот versioned checkpoint или создаёт новый.
