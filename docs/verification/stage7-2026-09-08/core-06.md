# CORE-06: конфигурация, роли и секреты

Дата: 10 сентября 2026 года. HEAD исходников при проверке: `dac12af`.
Локальная code/runbook проверка. **Целевой удалённый сервер недоступен**
(остаток BASE-01). Production-server пункты ниже явно открыты. Docker DB
suites и `make activity-ci` не запускались. Не выдавать этот файл за
проверку целевого хоста.

## Итог

| Пункт плана | Результат |
| --- | --- |
| Runtime/migration роли, запрет чужих owner DB, нет DDL у runtime | Локально подтверждено в `init-db.sh` и тестах границ. Изменение скрипта **не требуется**. Целевой сервер — открыто |
| Management listeners ограничены в целевом развёртывании | Код и Compose описаны. Привязка на целевом хосте — открыто |
| Startup validation pool/worker/deadline/retention/frame | Большая часть уже была; добавлены недостающие fail-fast проверки в `config.go` |
| Ротация encryption / integration / webhook / mTLS | Новый [runbook](../../runbooks/secrets-rotation.md). Автоперешифрования и CLI webhook-key нет |
| Логи/tracing/audit без JWT, OAuth tokens, текстов примечаний, закрытых ссылок | Утечек в разрешённых к правке пакетах не найдено. Правки логов не вносились |
| Production secret storage / KMS | **Не выбрано — нет доступа к целевому серверу.** Текущий keyring env/file для single-host сохранён. Клиент KMS не реализовывался. [ADR-0021](../../adr/0021-production-secrets.md) |

## 1. Роли и DDL — локально

Источник грантов: [`deploy/activity/init-db.sh`](../../../deploy/activity/init-db.sh)
(development fresh cluster). Существующие БД скрипт не сбрасывает.

| Роль | Назначение | CONNECT | DDL |
| --- | --- | --- | --- |
| `core_owner` / `activity_owner` / `events_owner` | Только миграторы | Своя БД (OWNER) | Да, объекты своей схемы |
| `core_runtime` / `activity_runtime` / `events_runtime` | API/worker/standalone | Только своя БД | Нет: `REVOKE CREATE ON SCHEMA public FROM PUBLIC`, нет GRANT CREATE |
| `PUBLIC` | — | `REVOKE ALL ON DATABASE` | CREATE отозван |

Default privileges owner → runtime: `SELECT, INSERT, UPDATE, DELETE` на таблицы
и `USAGE, SELECT` на sequences. Чужой owner DB: CONNECT выдан только парной
runtime-роли. Проверка конфигурации standalone:
`TestStandaloneRejectsForeignConfiguration` отклоняет `DATABASE_URL`,
`ENCRYPTION_KEYS`, `AMOCRM_CLIENT_SECRET` и чужой DSN.

Runtime DDL probe при старте: `componentruntime.openOwned` отклоняет
superuser/createdb/createrole и `CREATE` на `public`. Это Activity/CRM Events
и embedded owner pools. Core API/worker открывают пул через
`internal/platform/postgres.Open` **без** этого probe — гранты всё равно
запрещают DDL, но ошибочный `DATABASE_URL` с `*_owner` Core не отвергнет на
старте. Файл `postgres.go` вне ownership CORE-06; изменение не вносилось.

Миграции SQL лежат в `migrations/` и применяются контейнером `migrate` с
`*_owner` DSN. Runtime-процессы мигратор не запускают.

**Целевой сервер:** роли, гранты, `rolsuper`/`CREATE`, CONNECT к соседним БД
и фактический DSN runtime **не проверялись** (нет SSH).

## 2. Management listeners

| Процесс | Listener | Env | Default | Публикуется Compose |
| --- | --- | --- | --- | --- |
| Core API public | OAuth/widget/webhook | `HTTP_ADDRESS` | `:8080` | `127.0.0.1:8080` / activity `127.0.0.1:18080` |
| Core API management | `/live` `/ready` `/metrics` `/components` | `MANAGEMENT_HTTP_ADDRESS` | `:8082` | `127.0.0.1:8082` / `127.0.0.1:18082` |
| Core worker health | `/live` `/ready` `/metrics` `/components` | `HTTP_ADDRESS` | `:8081` | основной стек `127.0.0.1:8081`; activity-стек **не** публикует |
| Activity | health | `SERVICE_HEALTH_ADDRESS` | `:8091` | не публикуется |
| CRM Events | health | `SERVICE_HEALTH_ADDRESS` | `:8092` | не публикуется |

Код: conflict public vs management отклоняется; формат `host:port` проверяется.
Внутри контейнера default `:8082` слушает все интерфейсы; ограничение loopback
делает host port mapping. Код **не** требует loopback — иначе сломается
Docker DNS bind.

**Целевой сервер:** фактический bind, firewall и что management не торчит в
ingress **не проверялись**.

## 3. Startup validation

Уже было в `LoadAPI` / `LoadWorker` / `loadCommon` / `componentruntime.Load`
(изменение не требуется, кроме добавленного ниже):

- пулы: `DB_MAX_CONNS` 1..100; Activity/Events `*_DB_MAX_CONNS` 1..16;
  `CRM_EVENTS_WORKERS` 1..16 и пилот max 4;
- worker: poll ≥100ms, lease ≥3s, batch 1..100, reap 1..1000, concurrency
  1..64, integration concurrency 1..64;
- webhook/widget rate/burst — finite positive; webhook timeout < 2s;
  `MAX_WEBHOOK_BODY_BYTES` 1KiB..16MiB;
- retention inbox/delivery webhook — positive duration;
- Activity `initial_days` 1..7, `retention_days` 2..30 ≥ initial — на записи
  настроек (`serviceapi.ValidateSettings`), не env;
- ответ/frame: `MaxResponseBytes` 3MiB и gRPC `MaxMessageSize` 4MiB —
  compile-time константы, env нет;
- development encryption key запрещён вне `APP_ENV=development`;
- `PUBLIC_BASE_URL` обязателен у worker; HTTPS без path — при сборке
  webhook reconcile.

Добавлено в этом slice (fail-fast, defaults не менялись):

- разбор `ENCRYPTION_KEYS` через `cryptox.ParseKeyRing` уже в `Load*`;
- `HTTP_ADDRESS` — TCP `host:port`;
- `OAUTH_STATE_TTL` 1m..1h (default 15m);
- `WIDGET_JWT_LEEWAY` ≤1m, `WIDGET_JWT_MAX_LIFETIME` ≤1h и ≥ leeway
  (defaults 5s / 15m);
- `WORKER_JOB_TIMEOUT` ≥1s (default 45s).

OAuth limiter env-ключей CORE-03 в `config.go` на момент правки не было.
Если появятся — валидировать как finite positive rates в том же стиле.

Проверка без PostgreSQL: `go test ./internal/platform/config`.

## 4. Ротация

Новый runbook: [`docs/runbooks/secrets-rotation.md`](../../runbooks/secrets-rotation.md).
Кратко:

- Keyring: добавить версию → rolling restart → перешифровать известными
  операциями → удалять старую версию только когда SQL inventory пуст.
  Автоперешифрования нет; webhook ciphertext сам не обновляется при re-auth.
- Integration secret: существующий `integrations rotate-secret --secret-stdin`.
- Webhook destination key: CLI нет (CORE-02). Не удалять encryption version,
  пока `webhook_key_key_version` на неё ссылается.
- mTLS: `service-certs` помечен как development-only (stderr + комментарий).
  Production — свой CA, overlap сертификатов, без ослабления
  `RequireAndVerifyClientCert`.

## 5. Утечки секретов в логах

Поиск по runtime логам/ошибкам: `Authorization`, `access_token`,
`refresh_token`, `Bearer`, JWT, `client_secret`, webhook key, тела notes.

| Место | Наблюдение |
| --- | --- |
| `httpmiddleware.AccessLog` | method/route/status/bytes; **не** URL, query, headers |
| `widgetauth` | `reason_code` + безопасные match-флаги; тест `TestMiddlewareLogsSafeReasonAndRequestIDOnly` |
| `oauth.Handler` | `"error", err` + `request_id`; callback JSON без tokens (`InstallationResult`) |
| `webhook.Handler` | request_id / delivery_id / installation_id; ключ из path не логируется |
| `jobs.Worker` | job_id/type/attempt/installation_id; payload нет |
| `activitybridge` | command_id и error code, не payload |
| `componentruntime` | mode/rpc/health без DSN |
| `integrations` CLI | Result без secret/ciphertext; audit metadata без секретов |
| Tracing | отдельного OTel exporter нет |
| Notes / закрытые ссылки | тексты примечаний в логах не пишутся; panel `entityHref` даёт только относительный `/leads/detail/{id}` |

Известный прошлый дефект BUG-007 (webhook key в `url.Error`) закрыт.
`oauth/handler.go` принадлежит CORE-03 — не редактировался; текущая запись
ошибки не содержит token/code. Новых утечек, которые CORE-06 мог править,
не найдено.

## 6. KMS / хранение секретов

**Не выбрано — нет доступа к целевому серверу.** Не выдумывать AWS KMS / Vault.
Текущая схема для single-host: keyring в env/file, запрет development key вне
development, operator CLI с тем же кольцом. Клиент KMS не писался (Appendix G:
KMS не условие первого экрана). Решение зафиксировано в ADR-0021 как отложенная
граница, не как внедрение.

## 7. Целевой сервер — открыто

- [ ] SSH/алиас и каталог Compose целевого backend
- [ ] Фактические роли PostgreSQL, гранты CONNECT/CREATE, DSN runtime vs owner
- [ ] Management listeners: bind, firewall, отсутствие в публичном ingress
- [ ] `APP_ENV`, `ENCRYPTION_KEYS` без development key, источник секретов
- [ ] mTLS identities не от `service-certs` / не 30-дневные localhost
- [ ] Backup/restore ключей и overlap сертификатов
- [ ] Выбор KMS/secret store по инфраструктуре хоста
