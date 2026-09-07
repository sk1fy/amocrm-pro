# Activity v0: запуск, пилот и выделение процесса

Архитектурное задание: редакция v2 от 2026-09-06. Владельцы и зависимости:
[ADR-0010](../adr/0010-activity-v0-service-ownership.md). Этот runbook описывает
реализованные команды, а результаты их исполнения — отдельный
[отчёт проверки](../verification/activity-v0-results.md) и
[дополнение после первого аудита](../verification/activity-v0-audit-followup.md) и
[исправления второго аудита](../verification/activity-v0-hardening.md).

## Топологии и подготовка

`docker-compose.activity.yml` создаёт **отдельный development-стек** с собственным
volume PostgreSQL. Это не обновление существующего `docker-compose.yml`, сервера
или данных PHP. Все пароли/ключ шифрования в development Compose демонстрационные.
Для реальных данных подставьте собственные секреты, TLS/CA, backup policy,
ограниченный административный доступ и выбранную installation.

```sh
make activity-up
docker-compose -f docker-compose.activity.yml ps
curl --fail http://127.0.0.1:18082/ready
```

Fresh-cluster bootstrap создаёт `amocrm_core`, `amocrm_activity`, `amocrm_events`.
Роли `core_runtime`, `activity_runtime`, `events_runtime` имеют DML только своего
владельца, не имеют DDL и не могут CONNECT к соседним БД. Миграторы используют
`core_owner`, `activity_owner`, `events_owner`. Runtime сервисов дополнительно
отклоняет administrative/DDL-роль при старте. Bootstrap не предназначен для
повторного применения к существующей общей БД.

Контейнер сертификатов создаёт отдельные mTLS identities со сроком 30 дней:
`spiffe://amocrm-pro/core`, `/gateway`, `/activity`, `/crm-events`. CA private key
не сохраняется. Только Gateway/policy volume содержит Ed25519 delegation private
key. Каждый standalone получает только свой identity volume и свой DSN.
При истечении dev-сертификатов остановите этот dev-стек, удалите только четыре
identity volumes и повторите создание; PostgreSQL volume не удаляйте. Production
ротация выполняется средствами CA с периодом совместимости сертификатов.

| Процесс | DB pool | Рабочие слоты | Зависимости |
| --- | ---: | ---: | --- |
| Core API | Core 6 | outbox 1; RPC admission ограничен | Activity, Events, policy |
| Core worker/Gateway | Core 6 | Core jobs 2, integration cap 1 | amoCRM, Core |
| Activity | Activity 3 | до 32 unary RPC | Events, policy, Gateway users |
| CRM Events | Events 5 | 2 page workers; 1 backfill page глобально | policy, Gateway events |

Итого runtime до 20 DB connections, плюс до 3 краткоживущих миграционных и резерв
администрирования. После embedded-переключения сумма пулов сохраняется. Это
пилотные квоты для одной реплики каждого владельца. gRPC concurrency — отдельный
лимит от job slots, DB connections и amoCRM request budget. Перед увеличением
реплик пересчитайте все четыре ограничения.

Владелец исходящего Go API v4 бюджета — **один worker/Gateway**. Старые lead-status
и webhook reconciliation используют тот же `amocrm.Client`, что Gateway.
Activity/CRM Events получают только ограниченные Events/Users RPC. Чтение account
при OAuth также проходит через общий клиент: Core-only `CoreBootstrap.GetAccount`
проверяет integration из consumed OAuth state; временный access token остаётся
между процессами Core и не сохраняется Gateway. До определения первого account ID
действуют integration и общая discovery квоты, затем списывается canonical account
budget. Только token exchange/refresh `/oauth2/access_token`, PHP и другие клиенты
не входят в этот budget; 429 обрабатывается как контролируемая
ошибка с Retry-After. Не запускайте несколько активных Gateway без новой схемы
координации лимитов. Запрет произвольного HTTP proxy сохраняет ограниченный API.

Базовый стек остаётся с `ACTIVITY_MODE=off`, пока режим явно не включён. Отдельные
процессы требуют `ACTIVITY_MODE=grpc`. Общий `DATABASE_URL`, `ENCRYPTION_KEYS`,
`AMOCRM_CLIENT_SECRET` и соседний продуктовый DSN запрещены standalone-конфигурацией.
Гарантия единого Gateway budget относится к включённым Activity-топологиям.
Core-only/off сохраняет прежний прямой OAuth account lookup.

## Включение выбранной установки

Сначала provision integration через существующий audited CLI и пройдите её OAuth
по [общему runbook](integrations.md). В новом isolated development-стеке нет
автоматически скопированных installations или OAuth credentials.

```sh
docker-compose -f docker-compose.activity.yml run --rm integrations set-service \
  --actor operator@example.org --code YOUR_INTEGRATION --service activity --enabled true
docker-compose -f docker-compose.activity.yml run --rm activity-control \
  pilot-enable INSTALLATION_UUID
```

Capability integration и pilot installation независимы, по умолчанию pilot
выключен. Совпадение amoCRM account у разных integrations не даёт совместного
доступа к истории. Все endpoints v0 требуют подтверждённого сервером активного
администратора. Каждое новое обращение и доставка команды проверяют роль через
общий Gateway client без межзапросного кеша. Подписанный результат действует
до 30 секунд внутри начатого запроса; Validate на каждом переходе заново
проверяет Core DB capability/pilot/reauth. Отзыв роли в amoCRM блокирует следующий
новый запрос; уже допущенная цепочка может завершиться до истечения делегации.
См. [ADR-0011](../adr/0011-activity-authorization-budget.md). Group, name и timezone
могут кешироваться до 30 секунд. Frontend `is_admin` не учитывается.

Подключите [панель v0](../../examples/activity-v0/README.md) к установленному
виджету. Каждый browser-запрос использует новый disposable amoCRM JWT через
существующий `$authorizedAjax`, `X-Auth-Token`, CORS и общие widget limits.
Токены нельзя повторно использовать для polling. Поля installation/integration/
actor определяет Core, а `user_ids` — только фильтр сотрудников.

Публичные endpoints под `/api/v1/widget/activity`:

- `GET /panel?from=UNIX&to=UNIX&user_ids=1,2&limit=100&cursor=...`;
- `GET /status`, `GET /settings`, `POST /settings`;
- `POST /sync` с `kind=enable|sync|backfill|disable`;
- `GET /operations/{commandID}`.

Имена и типы полей — в [OpenAPI](../../api/openapi.yaml) и
[protobuf](../../api/proto/services.proto). Панель ограничена 31 днём, 100
сотрудниками и 100 событиями на страницу; общий JSON-ответ не больше 3 MiB
в обоих режимах (при превышении уменьшите страницу). Сводка рассчитывается одним запросом
для всего выбранного периода, независимо от страницы событий.

Первый `POST /sync` с `{"kind":"enable"}` и новым Idempotency-Key сохраняет
consumer и запускает загрузку по defaults: 2 дня истории, retention 7 дней.
Настройки Activity: initial 1–7 дней, retention 2–30 дней и не меньше initial.
Сохранённые настройки применяются к сборщику **следующей принятой sync-командой**;
между БД нет скрытой общей записи. Ручной `backfill` принимает `from/to`, не более
31 дня и не старше retention; он не закрывает непроверенный промежуток автоматически.

HTTP 202 означает durable приём Core. Пока получатель недоступен, операция имеет
`pending_delivery`; после подтверждения — `accepted`, затем статус владельца.
Не показывайте «синхронизация завершена» по одному HTTP 202. При потере ответа
повторите тот же Idempotency-Key с теми же параметрами и **новым JWT**. Другой
payload с тем же ключом даёт conflict. Доставка сохраняет command/operation ID.
Единственный успешный публичный статус — `succeeded`; остальные состояния владельца:
`accepted`, `running`, `retry`, `paused`, `failed`. Сохранённый CRM Events `completed`
нормализуется прикладным слоем; контракт старых Core jobs не изменён.

## Свежесть и восстановление

Events API запрашивается страницами по 100, с фиксированными часовыми окнами и
минутным перекрытием. Одна job slice — одна страница; каждый сетевой вызов
ограничен 10 секундами и выполняется вне write transaction. Lease — 30 секунд;
проверка fencing защищает запись после restart/потери lease. Default polling —
5 минут, owner cleanup — ограниченными порциями раз в минуту.

Два последовательных одинаковых прохода окна дают
`verification=stabilized_api_scan`; максимум три прохода. Изменяющаяся пагинация
и превышение лимита страниц оставляют явную ошибку и не продвигают checked range.
Это проверенный доступный API диапазон, а не обещание полной истории аккаунта.
Очень поздние события вне overlap могут требовать ручной догрузки.

Различайте retained history, continuous verified range, current window/page,
last success, last event, lag и reauth. `unknown`, `partial` и `stale` нельзя
представлять как доказанную нулевую активность. Retention удаляет события порциями,
но не отматывает cursor и не запускает бесконечную догрузку удалённой истории.
Очистка посещает один источник за минутный тик, до 1000 событий за вызов,
по очереди между источниками. Удаление и граница гарантированной сохранности
коммитятся атомарно; при частичном удалении старые строки ещё могут оставаться
за этой границей. Это не минимальная дата физически присутствующего события.
При N источниках полный круг занимает примерно N минут, без учёта занятых
источников; лимит очистки и её отставание нужно учитывать при росте нагрузки.

После простоя worker продолжает durable окно/страницу. После `reauth_required`
пройдите OAuth той же installation и отправьте новую `sync`; после exhausted
retries/pagination ошибки исправьте причину и отправьте `sync`. Удалять jobs,
inbox, source state или создавать новый operation ID для старой команды не нужно.

Outbox: максимум 20 попыток с backoff 2–256 секунд, без очистки receipt/inbox v0.
Permanent denial и исчерпание попыток видны как final failure. После исправления:

```sh
docker-compose -f docker-compose.activity.yml run --rm activity-control retry COMMAND_UUID
```

Повтор оператором аудируется, сохраняет identity и payload команды и снова
проверяет актуальные права. Receipt/inbox/job history пока не очищаются: срок
очистки должен покрывать согласованный retry/dedup horizon всех владельцев.

## Отключение и возврат

```sh
docker-compose -f docker-compose.activity.yml run --rm activity-control \
  pilot-disable INSTALLATION_UUID
```

Core pilot-disable запрещает чтения Activity и новые команды/порции. Policy
проверяется без кеша перед каждой новой порцией; недоступность policy не даёт
разрешение. Уже допущенная страница может закончиться в пределах 10-секундного
RPC плюс короткой записи до 5 секунд. Отдельная sync-команда `kind=disable`
приостанавливает **сбор consumer**, сохраняя право разрешённого чтения истории;
это не эквивалент Core pilot-disable. Другие продукты не отключаются.

V0 поддерживает единственного consumer Activity. Позднее другие потребители
должны получать явные policy grants; отключение Activity не должно удалять их
потребность или общую историю. Сейчас произвольный consumer отклоняется.
Независимость нескольких consumers сейчас не реализована и не проверена:
добавление второго потребует изменения schema, policy и выбора потребностей сборщиком.

Для возобновления включите capability/pilot, при необходимости повторите OAuth,
затем `sync`. Не восстанавливайте старый snapshot поверх новых записей.

## Переключение размещения без перемещения БД

1. Проверьте одинаковую совместимую версию образов/контрактов/миграций; зафиксируйте
   jobs/operations и метрики. Остановите новые widget-команды на maintenance edge.
2. Остановите API, затем CRM Events и Activity. Дайте graceful shutdown завершиться;
   после аварийной остановки дождитесь 30 секунд lease. Core worker/Gateway остаётся.
3. Для embedded запустите API с дополнительным Compose-файлом. Он создаёт только
   owner pools и локальные порты; Gateway/policy остаются удалёнными в worker:

```sh
docker-compose -f docker-compose.activity.yml stop api crm-events activity
docker-compose -f docker-compose.activity.yml -f docker-compose.activity-embedded.yml \
  up --detach api
```

4. Проверьте ready, панель, сохранённую operation, один повтор команды, новую
   синхронизацию и второй виджет. Возобновите входящие команды.
5. Возврат в grpc: остановите embedded API; запустите `crm-events activity api`
   только с `docker-compose.activity.yml`. Те же volumes/DSNs/миграции сохраняются.

Не выполняйте `down --volumes`, down-миграции, автоматический fallback или запуск
двух разных режимов scheduler одновременно. Fencing помогает при аварии, но не
заменяет контроль владельцев и общих квот. Независимый restart Activity не требует
нового Core release; совместимый contract/schema release всё равно обязателен.

## Диагностика и резервные копии

Management `GET /components` показывает фактически зарегистрированные порты,
режим размещения, владельца миграций, владение локальной БД и квоты без DSN/секретов.
Registry проверяет зависимости, режимы, интерфейсы и readiness при сборке;
после сборки граф закрыт для новых регистраций. `contract_version=v1` — версия
межсервисного контракта; `product_version=v0` — объём Activity/CRM Events.

`/live`, `/ready`, `/metrics` размещены на management listeners. Core `/ready`
проверяет сам Core; доступность необязательного графа Activity показана отдельно
на `/components/activity/ready`, чтобы отказ продукта не убирал другие виджеты
из ingress. Core grpc clients подключаются без блокирования старта, с bounded
ошибками при вызове недоступного владельца. Standalone
readiness проверяет собственную БД и необходимые RPC dependencies. Метрики
`crm_events_*`, `component_db_*`, `component_sql_duration_seconds`,
`service_rpc_*`, `amocrm_budget_wait_seconds`, `amocrm_requests_total` имеют
конечные labels. ID и payload в metric labels отсутствуют. Общий PostgreSQL
и Gateway остаются общими точками отказа.

```sh
docker-compose -f docker-compose.activity.yml logs --tail 100 api worker activity crm-events
docker-compose -f docker-compose.activity.yml exec -T crm-events \
  wget -qO- http://127.0.0.1:8092/metrics
```

Для ценных данных проверенный backup/restore каждого владельца обязателен до
пилота. Делайте `pg_dump --format=custom` соответствующей БД с backup-ролью,
шифруйте backup и тестируйте restore в **новую** БД с её own roles/migrations.
Сверяйте event count, source windows, operations и dedup, затем пробный restart.
Перенос DSN/инстанса — отдельная процедура; этот runbook переключает только
процессы. Наличие примера backup-команды не означает выполненную restore-проверку.
