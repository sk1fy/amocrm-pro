# Запустить Platform V1

## Локальный запуск

Запустите Docker и выполните команды из корня репозитория.

```sh
make platform-up
curl --fail http://127.0.0.1:18080/live
curl --fail http://127.0.0.1:18080/ready
```

`make platform-up` поднимает PostgreSQL, запускает одноразовый мигратор
и ждёт готовности Platform. При каждом вызове мигратор сверяет контрольные
суммы в `schema_migrations`. PostgreSQL, мигратор и Platform входят в отдельный
Compose-проект. Команда `make platform-down` останавливает проект и сохраняет
том PostgreSQL.

Локальный Compose публикует публичный HTTP и закрытый HTTPS на loopback.
Для другого локального запуска задайте `PLATFORM_COMPOSE_PROJECT`,
`PLATFORM_PUBLIC_PORT`, `PLATFORM_PRIVATE_PORT` и `PLATFORM_DEV_CERTS_DIR`.
PostgreSQL и management listener доступны внутри окружения Compose.

## Настроить PostgreSQL

Выделите логическую БД `amocrm_platform`. Используйте отдельный экземпляр
PostgreSQL или запретите ролям Platform подключаться к соседним БД.
Для локального запуска Compose создаёт роли и grants при инициализации
пустого тома. В другом окружении задайте собственные пароли и примените
те же ограничения. [Локальный SQL](../../deploy/platform/init.sql).

| Роль | Назначение |
| --- | --- |
| `platform_owner` | Владеет БД. Передайте её `DATABASE_URL` одноразовым утилитам `platform-migrate` и `platform-lifecycle bootstrap-platform`. Мигратор применяет SQL из `migrations-platform`. |
| `platform_access` | Читает `platform_access.runtime_probe`. Передайте её DSN как `PLATFORM_ACCESS_DATABASE_URL`. |
| `platform_jobs` | Обслуживает очередь и записывает lifecycle inbox, проекции и сверку. Продолжение обработки lifecycle ставится в очередь через отдельную SQL-функцию. Передайте её DSN как `PLATFORM_JOBS_DATABASE_URL`. |

Включите проверку сертификата PostgreSQL для удалённого подключения
через `sslmode=verify-full`. Перед стартом проверьте общий бюджет соединений
по числу реплик. Значения по умолчанию для одной реплики составляют четыре
соединения access и четыре соединения jobs. Переменные
`PLATFORM_ACCESS_POOL_MAX_CONNS` и `PLATFORM_JOBS_POOL_MAX_CONNS` принимают
от 1 до 16. У мигратора отдельное соединение. При откате приложения оставьте
применённую схему и используйте совместимую версию образа.

## Настроить listeners и mTLS

| Listener | Адрес по умолчанию | Назначение |
| --- | --- | --- |
| `PLATFORM_PUBLIC_HTTP_ADDRESS` | `:8080` | `GET /live` и `GET /ready`. |
| `PLATFORM_PRIVATE_HTTP_ADDRESS` | `:8443` | HTTPS с обязательной проверкой клиентского сертификата. |
| `PLATFORM_MANAGEMENT_HTTP_ADDRESS` | `127.0.0.1:8081` | `GET /metrics`, `GET /live` и `GET /ready`. |

`/live` проверяет, что HTTP listener отвечает. `/ready` выполняет чтение
через access-пул с таймаутом в одну секунду. При отказе этого чтения
`/ready` отвечает 503. Приём lifecycle и очередь используют пул jobs.

Выдайте Platform серверный сертификат с DNS SAN имени закрытого listener.
Передайте пути в `PLATFORM_TLS_CERT_FILE`, `PLATFORM_TLS_KEY_FILE` и
`PLATFORM_TLS_CLIENT_CA_FILE`. Сохраняйте TLS до процесса Platform.
Ограничьте доступ к закрытому и management listeners на сетевом уровне.

Для `/internal/platform/v1/core-events` Platform требует один URI SAN
`spiffe://amocrm-pro/core`. Для `/internal/platform/v1/managers/*` требуется
один URI SAN `spiffe://amocrm-pro/manager-bff`. Клиент из другой группы
получает 403. События принимаются только по `POST`. Менеджерские операции пока возвращают 404.
Бизнес-операции описывает [контракт Platform V1](../platform-v1-contract.md).

`make platform-certs` выпускает 30-дневный локальный комплект в
`tmp/platform-certs`. Core и Platform используют свои сертификаты для входящей и исходящей сторон mTLS. В `core` также создаётся локальный ключ `snapshot-delegation.key`. Manager BFF использует клиентский сертификат. Комплекты P02 выпущены с другими EKU. Для локальной проверки P03 задайте новый каталог `PLATFORM_DEV_CERTS_DIR`.

## Проверить очередь

`PLATFORM_WORKERS` задаёт число воркеров на реплику. Значение по умолчанию
равно двум. Для продолжения применения lifecycle зарегистрирован обработчик `lifecycle.drain`. Воркер выполняет
попытку с контекстом на 30 секунд и получает lease на 45 секунд. После ошибки
воркер возвращает задачу с задержкой от секунды до минуты. После последней
попытки задача получает статус `dead`. Просроченный lease освобождается
пакетами до 100 строк. Завершение проверяет номер попытки и срок lease.
Обработчик внешнего эффекта обязан поддерживать повтор по устойчивому ID
задачи. Терминальные строки остаются доступны для разбора.

`platform_queue_depth`, `platform_queue_oldest_ready_seconds` и
`platform_queue_expired_leases` показывают последний снимок из PostgreSQL.
`platform_queue_snapshot_ok` становится равной 0, если снимок снять не
удалось. `platform_queue_outcomes_total` и `platform_queue_database_errors_total`
считают события одной реплики.

`make platform-test` запускает одноразовую БД и проверяет повтор и конкурентный
запуск мигратора, права ролей, отказ запуска, mTLS, независимость readiness при
занятом jobs-пуле, завершение по SIGTERM и восстановление lease. Для проверки
обработчиков тест регистрирует свой вид задачи.


## Включить пилот lifecycle

По умолчанию сопоставления интеграций и список пилотных аккаунтов пусты.
Примените миграции Core и Platform. В закрытой сети передайте воркеру Core
`PLATFORM_DELIVERY_ORIGIN` с HTTPS origin Platform, например
`https://platform:8443`, и `PLATFORM_CORE_IDENTITY_DIR` с `ca.crt`,
`tls.crt` и `tls.key`. Worker требует один URI SAN
`spiffe://amocrm-pro/platform` у сервера Platform. Core API принимает
`PLATFORM_SNAPSHOT_RPC_ADDRESS` для отдельного приватного gRPC listener,
тот же каталог identity и `PLATFORM_SNAPSHOT_KEY_FILE` с отдельным PKCS#8
PEM ключом Ed25519. На клиенте Platform установите проверку TLS для
имени Core и его CA. Activity может быть выключен.

Утвердите один manifest для обеих баз. Сверьте UUID интеграций с Core.
IDs аккаунтов укажите в десятичных строках. Например:

```json
{
  "version": 1,
  "integrations": [
    {
      "integration_id": "eb48bba7-e53a-4d58-a139-813f4dc59788",
      "product_code": "example",
      "amo_account_ids": ["9007199254740993"]
    }
  ]
}
```

Соберите отдельную операторскую утилиту из target `platform-lifecycle`.
Передайте один `DATABASE_URL` владельца на вызов. Сначала примените
manifest у Platform, затем у Core. Сравните напечатанные SHA-256 digest.

```sh
DATABASE_URL="$PLATFORM_OWNER_DSN" platform-lifecycle bootstrap-platform pilot.json
DATABASE_URL="$CORE_OPERATOR_DSN" platform-lifecycle bootstrap-core pilot.json
```

Порядок интеграций и аккаунтов в manifest не меняет digest. Повтор того
же manifest безопасен. Перезапись принятой версии отключена. Начальная
загрузка исторических поколений и управление составом выпуска относятся
к P10. Существующие установки из пилота попадают в сверку Core
`history_unproven`. Новый OAuth для такой строки не открывает первый эпизод.

## Разобрать пропуск или незавершённое удаление

- В Core проверяйте `platform_lifecycle_reconciliation` и состояние
  доставки в `platform_lifecycle_ledger`, `platform_lifecycle_deliveries`
  и `jobs`. В Platform проверяйте незакрытые строки
  `platform_lifecycle.reconciliation`, а также `cursors` и `inbox`
  в той же схеме. Для другой базы подключайтесь отдельно.
- Воспроизведите диапазон исходных событий одной установки командой
  `platform-lifecycle replay INSTALLATION_UUID FIRST LAST` с DSN Core.
  Допускается до 100 последовательных позиций. Если хотя бы одной
  позиции нет в журнале, вся команда отклоняется. Обычный worker
  повторно передаёт прежний UUID и исходные байты. Журнал и подтверждения
  доставки хранятся независимо от retention Core jobs.
- `sequence_gap` исчезает после применения недостающих записей.
  Если накопилось больше 24 применимых событий, Platform ставит задачу
  `lifecycle.drain` в своей очереди. Если эта задача исчерпала повторы,
  повторите доставку исходного события. Сверьте курсор и незакрытые
  строки. Конфликты scope и занятой позиции требуют разбора у владельца.
- Для чтения актуального состояния в Core выпустите 30-секундное
  делегирование через `platform-lifecycle snapshot-delegation INSTALLATION_UUID INTEGRATION_UUID`.
  Утилита берёт ключ из `PLATFORM_SNAPSHOT_KEY_FILE`. Передайте результат
  как `GetSnapshotRequest.auth` через закрытый mTLS. Не сохраняйте
  токен в командных логах. RPC повторно проверяет сопоставление и
  возвращает состояние отключённой установки в разрешённой области.
  Snapshot не продвигает курсор и не дополняет журнал событиями.
- `uninstall_pending` означает, что Core закрыл авторизацию и ждёт
  доказательства удаления. Дождитесь завершения выполняющегося
  `webhook.reconcile` для этой установки. Повторите uninstall с прежним
  текущим состоянием. Core подтверждает удаление после чтения полного
  списка amoCRM и проверки отсутствия принадлежащих установке URL.
  Неполный ответ, отсутствие учётных данных и неопределённый исход
  оставляют операцию на сверке. Если приходит новый OAuth, попытка
  вывода из эксплуатации завершает действие и меняется ключ URL.
  Уже отправленный внешний запрос amoCRM может завершиться позднее.
  Обработка ограничена старыми подтверждёнными URL. Удаление с прежним
  UUID попытки не может зафиксировать исход для новой авторизации.
  Если ответ на уже отправленный запрос регистрации был потерян за
  пределами жизни Core job, amoCRM может подтвердить этот запрос после
  повторного списка. Повторный reconcile использует реестр URL и
  текущий ключ. Историю с неизвестным прежним ключом сверяйте вручную.

Срок и его источник хранятся на паре аккаунт/продукт. P03 создаёт
эпизоды и историю. Политику сроков, выдачу proof и публичный доступ
включает P04.

## Проверить доставку lifecycle локально

Запустите `make platform-lifecycle-test` из корня репозитория. Команда
создаёт две отдельные PostgreSQL 17, применяет миграции каждого владельца
и выпускает тестовые сертификаты. Тест проверяет откат OAuth при отказе
очереди, запись событий по mTLS, повторы, пропуски и новый эпизод с прежним
сроком. Проверка P02 по-прежнему запускается через `make platform-test`.
