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
| `platform_access` | Читает `platform_access.runtime_probe` и `platform_access.current_access`. Передайте её DSN как `PLATFORM_ACCESS_DATABASE_URL`. |
| `platform_jobs` | Обслуживает очередь, записывает lifecycle inbox, проекции, сверку и команды менеджеров. Продолжение обработки lifecycle ставится в очередь через отдельную SQL-функцию. Передайте её DSN как `PLATFORM_JOBS_DATABASE_URL`. |

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
| `PLATFORM_PUBLIC_HTTP_ADDRESS` | `:8080` | `GET /live`, `GET /ready` и публичный access при включённом пилоте. |
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
равно двум. При `PLATFORM_WORKERS=0` процесс не запускает очередь и не восстанавливает lease. Закрытый listener продолжает принимать события. Для продолжения применения lifecycle зарегистрирован обработчик `lifecycle.drain`. Воркер выполняет
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

## Включить пилот access

Примените миграции обеих БД и настройте общий lifecycle manifest.
Задайте политику точного продукта до подтверждения первой установки.
Подключитесь к БД Platform как `platform_owner`, укажите в SQL фактический
`products.code`, версию и ссылку на утверждённую карточку продукта.
Запрос должен вернуть одну строку.

```sh
psql "$PLATFORM_OWNER_DSN" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO platform_lifecycle.initial_term_policies(product_id, rule, version, evidence)
SELECT id, 'top_90d', 1, 'catalog/example/approved-v1'
FROM platform_lifecycle.products
WHERE code = 'example'
RETURNING product_id, rule, version;
SQL
```

| Правило | Срок от подтверждённого `installed.occurred_at` в UTC |
| --- | --- |
| `free` | Плюс 10 календарных лет. 29 февраля переходит на 28 февраля, если итоговый год невисокосный. |
| `top_90d` | 90 суток. |
| `pro_7d`, `pro_14d` | 7 или 14 суток согласно подтверждённой версии карточки этого продукта. |

Platform сохраняет правило, версию, ссылку на карточку, время и ID
исходного события вместе со сроком. Повторы, reconnect и reinstall
сохраняют запись. Пары, уже применённые в P03 или появившиеся без
утверждённой политики, автоматически срока не получают. Для будущих
установок меняйте правило, ссылку и версию вместе через `platform_owner`.
Роли `platform_jobs` и `platform_access` не могут менять политику.

Создайте отдельный Ed25519 ключ proof. Храните закрытый PKCS#8 только
у Core. Передавайте Platform только публичный SPKI PEM. Используйте
другие ключи для mTLS и snapshot delegation.

```sh
proof_dir="$(pwd)/tmp/platform-access-pilot"
mkdir -p "$proof_dir/core" "$proof_dir/platform"
(umask 077; openssl genpkey -algorithm ED25519 -out "$proof_dir/core/pilot.key")
openssl pkey -in "$proof_dir/core/pilot.key" -pubout -out "$proof_dir/platform/pilot.pub"
```

При запуске бинарников передайте Core `CORE_PLATFORM_ACCESS_PROOF_KID=pilot`
и `CORE_PLATFORM_ACCESS_PROOF_KEY_FILE=$proof_dir/core/pilot.key`.
Передайте Platform `PLATFORM_ACCESS_PROOF_PUBLIC_KEYS` с JSON-объектом
`{"pilot":"/absolute/path/platform/pilot.pub"}`, а также DSN access/jobs
и свой mTLS-комплект. По умолчанию оба маршрута выключены. Для браузера
задайте один точный HTTPS origin amoCRM в `PLATFORM_ACCESS_PILOT_ORIGIN`.
Запросы без Origin используют bearer proof. Синхронизируйте часы Core
и Platform, потому что проверка proof не даёт дополнительного времени.

Пример для локального Compose с уже настроенными БД, manifest и доставкой.
Остановите обычный контейнер каждого listener, если он занимает тот же порт.
Выполните команды в разных терминалах. Перед запуском Platform задайте
`PLATFORM_DEV_CERTS_DIR` с комплектом сертификатов из раздела mTLS.

```sh
proof_dir="$(pwd)/tmp/platform-access-pilot"
docker-compose -f docker-compose.yml run --build --rm --no-deps --service-ports \
  --user "$(id -u):$(id -g)" \
  -e CORE_PLATFORM_ACCESS_PROOF_KID=pilot \
  -e CORE_PLATFORM_ACCESS_PROOF_KEY_FILE=/access-keys/pilot.key \
  -v "$proof_dir/core:/access-keys:ro" api
```

```sh
proof_dir="$(pwd)/tmp/platform-access-pilot"
PLATFORM_UID="$(id -u)" PLATFORM_GID="$(id -g)" \
  docker-compose -p "${PLATFORM_COMPOSE_PROJECT:-amocrm-platform}" -f docker-compose.platform.yml \
  run --build --rm --no-deps --service-ports \
  -e 'PLATFORM_ACCESS_PROOF_PUBLIC_KEYS={"pilot":"/access-keys/pilot.pub"}' \
  -e PLATFORM_ACCESS_PILOT_ORIGIN=https://example.amocrm.ru \
  -v "$proof_dir/platform:/access-keys:ro" platform
```

### Проверить чтение и ротацию

Отправьте свежий одноразовый JWT Web SDK в `X-Auth-Token` или `Authorization: Bearer`
на `POST /api/v1/widget/platform-access-proof`. Сохраните значение `proof`
в памяти клиента. Отправьте его как bearer на
`GET /v1/access/{amo_account_id}/{product_code}`. Оба ответа запрещают кэширование.
Повторная проверка использует тот же proof до пяти минут, пока совпадают
пара, установка, поколение, версия авторизации и публичный ключ.

Для ротации добавьте новый публичный ключ с новым `kid` в карту проверяющих
реплик Platform и перезапустите их. Затем переведите Core на новую пару
`kid` и закрытого ключа. Через пять минут после последней выдачи старым ключом
уберите его из карты и перезапустите Platform. При отзыве ключа уберите его
сразу. Не используйте один `kid` для разных ключей.

После доставки отзыва следующий локальный read отклоняет прежний proof.
Если доставка Core остановлена, ещё не доставленный отзыв ограничен
истечением выданного proof. При истечении собственного срока
`active=false`, а `own_until`, `valid_until` и `source` сохраняют его
границу и источник. При отсутствующем сроке обе даты равны `null`.
Отказ PostgreSQL возвращает 503 с `Retry-After`.

## Проверить доставку lifecycle и access локально

Запустите `make platform-lifecycle-test` из корня репозитория. Команда
создаёт две отдельные PostgreSQL 17, применяет миграции каждого владельца
и выпускает тестовые сертификаты. Тесты проверяют откат OAuth при отказе
очереди, запись событий по mTLS, повторы, пропуски и новый эпизод с прежним
сроком. Тест P04 выдаёт proof через widget admission Core и читает его в
отдельном процессе Platform с `PLATFORM_WORKERS=0`. Admin и Gateway в
этом сценарии не запускаются. Тест останавливает Core HTTP, проверяет
истечение срока, ротацию ключей и отличает отказ PostgreSQL от неактивного
доступа. В журнале теста выводится локальное время от начала доставки
до первого отказа и от receipt до отказа. Рабочие TTL и SLA определяются
отдельно. Проверка P02 запускается через `make platform-test`.

## Включить очередь установок для Manager BFF

Примените миграции Platform и загрузите утверждённый manifest lifecycle.
Задайте `PLATFORM_MANAGER_CURSOR_KEY` с 32 случайными байтами в base64url
без padding. Передайте один ключ всем репликам Platform через хранилище
секретов. Без ключа маршруты менеджеров возвращают 404. Замена ключа требует
начать просмотр списка заново. Сохранённые команды и их исходы остаются доступны.
Для локального Compose задайте ключ перед `make platform-up`:

```sh
export PLATFORM_MANAGER_CURSOR_KEY="$(openssl rand -base64 32 | tr '/+' '_-' | tr -d '=\n')"
make platform-up
```

Укажите `PLATFORM_MANAGER_TIME_ZONE`, если бизнес-зона отличается от
`Europe/Moscow`. Используйте одно имя зоны IANA на всех репликах. Даты `from`
и `to` включают весь календарный день в этой зоне.

Передайте BFF клиентский сертификат `manager-bff` и CA закрытого listener.
Список требует `clients.read`. Review и чтение своего receipt требуют
`clients.write`. Пара аккаунта и продукта доступна только при однозначном
сопоставлении в Platform. До подключения источников активация возвращает
`unavailable`, CRM возвращает `unsupported`. Срок пары сохраняется для
закрытого эпизода и при переустановке. Авторизация относится к эпизоду.

Подтверждённые название и линейку продукта внесите под `platform_owner`
в `platform_lifecycle.products.display_name` и `platform_lifecycle.products.line`.
Без названия список показывает код продукта. Фильтр `line` включает только
продукты с явно указанной линейкой `FREE`, `TOP` или `PRO`. Поиск `q`
сопоставляет введённый текст с внешним ID аккаунта, кодом или названием
продукта. Контакты в поиск не входят.

Многостраничный результат хранится в PostgreSQL пять минут. Новая выдача
сохраняет до четырёх снимков сотрудника и вытесняет старейший из них.
В базе одновременно хранится до 32 снимков; при заполнении лимита Platform
возвращает 429 с `Retry-After`. При создании снимка Platform удаляет до
четырёх просроченных снимков. Если фильтр находит более 10 000 эпизодов,
уточните фильтр после ответа 400 `query_too_broad`. При 400 `invalid_cursor`
повторите запрос без курсора. На следующей странице передайте курсор, прежние
фильтры, лимит и сортировку. Одностраничные ответы не занимают место
в хранилище снимков.

Для повторной отправки review сохраните прежние `command_id`, `Idempotency-Key`,
`If-Match` и тело. Точный повтор возвращает первоначальные статус, тело и
`ETag`, если он был. Новая команда с устаревшим `If-Match` возвращает
`409 version_conflict` с `current_version`. При установке прежней отметки
с текущим `If-Match` Platform сохраняет исход команды без новой версии и
события. При смене отметки одна транзакция сохраняет ревью, исход, историю и
outbox с общим ID события. Причина остаётся во внутренней истории.
Синхронный receipt содержит `completed` или `rejected` без `operation_id`.
Если receipt ещё не найден, проверьте исход точным повтором команды.

Запустите `make platform-test` для локальной проверки двух процессов Platform
и PostgreSQL 17. Тест передаёт lifecycle через Core mTLS, читает снимки и
отправляет review через Manager BFF mTLS. Он проверяет повтор после
рестарта, конфликт версии, ограничения снимков и откат при отказе outbox.

## Подключить наблюдения для раздела «Клиенты»

Сначала примените миграции обоих владельцев и загрузите одобренную область
lifecycle. В закрытой сети задайте воркеру Core
`PLATFORM_ACCOUNT_GATEWAY_GRPC_ADDRESS`, например `:9094`, и передайте
`PLATFORM_CORE_IDENTITY_DIR` с `ca.crt`, `tls.crt` и `tls.key`. Сертификат
Core содержит URI SAN `spiffe://amocrm-pro/core` и DNS SAN для закрытого
адреса воркера. Endpoint принимает только Platform SPIFFE. Воркер использует
тот же бюджет amoCRM, что и его остальные операции.

Передайте Platform `PLATFORM_ACCOUNT_GATEWAY_ADDRESS` с адресом воркера,
`PLATFORM_ACCOUNT_GATEWAY_IDENTITY_DIR` с клиентским сертификатом
`spiffe://amocrm-pro/platform` и теми же именами файлов. При использовании
отдельного TLS-имени укажите `PLATFORM_ACCOUNT_GATEWAY_SERVER_NAME`.
Для автоматических и ручных обновлений нужен хотя бы один воркер Platform.
Публичный access и менеджерские чтения используют свои проекции PostgreSQL.

Чтение аккаунта и пользователей требует `clients.read`. Запросите refresh
с `clients.write`, исходными `command_id` и `Idempotency-Key` и списком
компонентов `license`, `users` или обоими. После 202 опрашивайте URL из
`Location`. Каждый автор сохраняет свой ID операции. Запрос может
присоединиться к уже запущенному обновлению того же компонента. При потере
ответа используйте receipt или повторите те же ключ, ID и тело.

Имя, домен и часовой пояс происходят из подтверждённого текущего аккаунта
amoCRM. Список пользователей и счётчик публикуются вместе после полного
ответа. При неполном ответе или отказе сохраняются прежнее наблюдение и
его время со статусом `stale`. Если пригодного наблюдения ещё нет,
Platform возвращает `unknown`. `unsupported` обозначает отсутствие
подтверждённого источника лицензии, лимита мест и amoSTART. История
лицензий содержит только записи доказательств. Список алиасов включает
домены, смена которых наблюдалась через Core.

`make platform-test` проверяет список, состояние операций и конкурентное
обновление на двух процессах Platform. `make platform-lifecycle-test`
проверяет доступ к реальному listener Core Gateway с PostgreSQL и тестовым
HTTPS-источником amoCRM.
