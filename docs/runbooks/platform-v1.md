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
| `platform_owner` | Владеет БД. Передайте её `DATABASE_URL` только одноразовому образу `platform-migrate`. Он применяет SQL из `migrations-platform`. |
| `platform_access` | Читает `platform_access.runtime_probe`. Передайте её DSN как `PLATFORM_ACCESS_DATABASE_URL`. |
| `platform_jobs` | Читает очередь и обновляет статус, время запуска, попытку и срок lease. Передайте её DSN как `PLATFORM_JOBS_DATABASE_URL`. |

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
`/ready` отвечает 503. Пул jobs обслуживает только очередь.

Выдайте Platform серверный сертификат с DNS SAN имени закрытого listener.
Передайте пути в `PLATFORM_TLS_CERT_FILE`, `PLATFORM_TLS_KEY_FILE` и
`PLATFORM_TLS_CLIENT_CA_FILE`. Сохраняйте TLS до процесса Platform.
Ограничьте доступ к закрытому и management listeners на сетевом уровне.

Для `/internal/platform/v1/core-events` Platform требует один URI SAN
`spiffe://amocrm-pro/core`. Для `/internal/platform/v1/managers/*` требуется
один URI SAN `spiffe://amocrm-pro/manager-bff`. Клиент из другой группы
получает 403. На этапе P02 разрешённая identity получает 404 на этих путях.
Бизнес-операции описывает [контракт Platform V1](../platform-v1-contract.md).

`make platform-certs` выпускает 30-дневный локальный комплект в
`tmp/platform-certs`. Подкаталоги `core` и `manager-bff` содержат клиентские
ключи соответствующих сервисов. Контейнер Platform получает только
подкаталог `platform`. Для другого окружения выпустите сертификаты его CA.

## Проверить очередь

`PLATFORM_WORKERS` задаёт число воркеров на реплику. Значение по умолчанию
равно двум. P02 запускает обслуживание очереди с пустым реестром
продуктовых обработчиков. При регистрации обработчика воркер выполняет
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
