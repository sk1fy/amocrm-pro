# ADR-0023: физический перенос продуктового модуля

- **Status:** принято как эксплуатационное правило. Локальный Docker-стенд с
  отдельными сетями — доказательство процедуры, не проверка целевого сервера.
- **Date:** 2026-09-11.

## Контекст

[ADR-0010](0010-activity-v0-service-ownership.md) и
[ADR-0012](0012-separable-product-module-contract.md) уже разделяют владельцев,
логические БД и embedded/gRPC composition. Перенос не должен менять прикладную
логику, публичный Core origin или код виджета. Этап 8 требует воспроизводимого
переноса Activity и независимо CRM Events.

Два разных сценария нельзя смешивать:

1. **Только процесс** — тот же owner DB, другой compute.
2. **Перенос БД владельца** — другой PostgreSQL-инстанс, отдельный backup/restore.

Горизонтальное масштабирование нескольких экземпляров одного владельца (leases,
общий бюджет amoCRM, общее хранилище) **не входит** в этот ADR. Gateway
остаётся единственным владельцем исходящего бюджета ([ADR-0019](0019-gateway-outbound-budget.md),
CORE-04).

## Решение

### Адреса и сеть

Процессы используют **статические** `host:port`: `GATEWAY_ADDRESS`,
`ACTIVITY_ADDRESS`, `CRM_EVENTS_ADDRESS`, `ACTIVITY_DATABASE_URL`,
`CRM_EVENTS_DATABASE_URL`. Dynamic discovery нет. Hostname из адреса —
TLS ServerName. Закрытые маршруты задаются firewall/Compose-сетями заранее:
продуктовый процесс не получает маршрута к чужому PostgreSQL, Core не получает
продуктовый DSN.

Локальный аналог разных серверов — overlay
[`docker-compose.activity-transfer.yml`](../../docker-compose.activity-transfer.yml):
плоскости `core-plane`, `activity-plane`, `events-plane`, `rpc-plane` и алиасы
`core-postgres` / `activity-postgres` / `events-postgres`. RPC-имена остаются
`worker`, `activity`, `crm-events`, потому что так устроены development-сертификаты.

### mTLS и сертификаты

`cmd/service-certs` выпускает 30-дневные development-сертификаты с DNS
сервиса/`localhost` и алиасами `api`/`worker`. Это **не production CA**. Для
нового размещения:

- выпустить leaf с SAN = фактический DNS (или IP) статического адреса;
- SPIFFE URI прежний (`spiffe://amocrm-pro/{core,gateway,activity,crm-events}`);
- `RequireAndVerifyClientCert` не отключать; `InsecureSkipVerify` не включать;
- CA private key не монтировать в runtime;
- ротация с перекрытием `NotAfter` — [secrets-rotation.md](../runbooks/secrets-rotation.md).

Нельзя «починить» переезд, ослабив TLS или продлив TTL `service-certs`.

### Drain, fencing, переключение

Пока работают два writer'а одного owner DB, возможна неучтённая двойная работа.
Порядок:

1. Остановить приём новых widget-команд на edge.
2. Дождаться пустого outbox/очереди или зафиксировать остаток.
3. SIGTERM процесса; graceful stop RPC до 5 с, Compose `stop_grace_period` 20 с.
4. После аварийной остановки подождать истечения lease CRM Events (30 с) и
   enrichment lease; не запускать второй scheduler на той же БД раньше.
5. Сменить статический адрес у всех вызывающих и перезапустить клиентов:
   для Activity — Core API; для CRM Events — Core API **и Activity**.
6. Проверить `/ready` нового процесса, затем `/live` Core. Core `/ready` не
   зависит от продукта.
7. Возобновить команды. Повтор того же command ID должен попасть в прежний
   inbox, а не создать вторую operation.

Откат compute-only: вернуть прежние адреса и тот же DSN, не делая restore.

### Перенос БД

Отдельные dump Core, Activity и Events **не являются** распределённым снимком.
Порядок:

1. Оградить writers/leases (см. выше).
2. `pg_dump --format=custom` только БД владельца ролью owner/backup.
3. Restore в **новую** БД на целевом инстансе с теми же owner/runtime ролями.
4. Финальный dump после fencing — точка cutover.
5. Переключить DSN процесса на новый хост; старый инстанс оставить read-only
   до подтверждения.
6. Rollback: вернуть DSN на прежний инстанс, **если** на новом не было записей.
   Данные, записанные после cutover, нужно отдельно перенести обратно либо
   признать потерянными; down-миграция их не вернёт.

Activity и CRM Events переносятся **независимо**. Публичный Core origin не
меняется. Виджет по-прежнему ходит в Core.

### Один экземпляр vs несколько

Вынос **одного** процесса каждого владельца — предмет этого ADR. Несколько
реплик Activity или CRM Events требуют отдельной проверки leases, fencing и
квот. Несколько Gateway — отдельное решение CORE-04; здесь запрещено.

## Последствия

- перенос — конфигурация и размещение, не новая бизнес-логика;
- при полном аварийном restore RPO определяется полным согласованным набором
  dump, снятым при остановленных writers всех владельцев; обязательная сверка
  accepted/receiver identity и отказ от несогласованного набора описаны в
  [backup runbook](../runbooks/activity-backup-restore.md);
- целевой физический multi-host и доступ к ключам production остаются отдельной
  проверкой.
