# Перенос Activity и CRM Events

Архитектурное правило: [ADR-0023](../adr/0023-product-module-physical-transfer.md).
Данные и RPO/RTO: [activity-backup-restore.md](activity-backup-restore.md).
Пилот и origin виджета: [activity-v0.md](activity-v0.md). Сертификаты:
[secrets-rotation.md](secrets-rotation.md).

Локальный overlay — [`docker-compose.activity-transfer.yml`](../../docker-compose.activity-transfer.yml).
Без этого файла default [`docker-compose.activity.yml`](../../docker-compose.activity.yml)
не меняется. Имена compose-проектов репетиции оканчиваются на `-test`.
Проект `amocrm-activity` и его runtime-БД не являются целью этой процедуры.

Целевой удалённый сервер в этой репетиции **не проверялся**.

## Два сценария

### A. Только compute (своя БД остаётся)

1. Зафиксировать версию образов, `schema_migrations`, метрики, незавершённые
   operations.
2. Остановить приём новых widget-команд.
3. SIGTERM Activity **или** CRM Events (независимо). Дождаться graceful
   shutdown. При kill — пауза 30 с на lease.
4. Запустить тот же бинарь на новом хосте с **тем же DSN** и новым
   статическим `SERVICE_RPC_ADDRESS` / DNS, который совпадает с SAN
   сертификата.
5. Переключить всех клиентов переносимого сервиса:
   - Activity: изменить `ACTIVITY_ADDRESS` у Core API и перезапустить API.
   - CRM Events: изменить `CRM_EVENTS_ADDRESS` **у Core API и standalone Activity**,
     перезапустить Activity, затем API. Activity создаёт свой Events gRPC-клиент
     при старте; изменение переменной только у Core его не переключает.
   `DATABASE_URL` Core, encryption keyring и публичный HTTP origin сохраняются.
6. Проверить `/ready` продукта и Activity, затем через прежний Core origin:
   panel, карточку события, settings, consumer enabled и прежний operation ID.
   Старый Events endpoint должен быть выключен во время проверки, иначе
   успешный ответ может скрыть непереключённого клиента.
7. Откат: вернуть прежние адреса **у всех клиентов из шага 5** и перезапустить
   их, используя ту же БД. Restore не нужен, если писать начал только один процесс.

Если сохраняется прежнее DNS-имя, вместо env переключается его запись.
Проверить SAN, истечение DNS-кеша и переподключение обоих клиентов при
недоступном старом endpoint; при необходимости перезапустить клиентов.

### B. Перенос owner DB на другой PostgreSQL

Дополнительно к A:

1. Поднять целевой инстанс с теми же `*_owner` / `*_runtime`.
2. После fencing — dump источника, restore в новую БД на цели.
3. Финальный dump (cutover). Допустимый простой = dump + restore + смена DSN +
   `/ready` (локально см. `RTO_TRANSFER_DB_SECONDS`).
4. Процесс стартует с новым DSN (`events-postgres-target` в overlay).
5. Старый инстанс не принимает writers. Сохранить его до проверки.
6. Rollback: DSN назад, **если** на цели не было новой записи. Иначе
   перенести хвост вручную либо принять потерю. Down SQL хвост не вернёт.

Activity и Events переносятся по очереди, не одним шагом. Сначала можно
вынести Activity (настройки), затем независимо CRM Events (история).

## Сеть, mTLS, DSN

Статические адреса уже читаются в
[`internal/componentruntime/config.go`](../../internal/componentruntime/config.go).
Standalone отвергает чужой DSN и Core secrets. Overlay задаёт:

| Плоскость | Кто | Имена |
| --- | --- | --- |
| `core-plane` | api, worker, postgres Core | `core-postgres` |
| `activity-plane` | activity, postgres Activity | `activity-postgres`, цель `activity-postgres-target` |
| `events-plane` | crm-events, postgres Events | `events-postgres`, цель `events-postgres-target` |
| `rpc-plane` | api, worker, activity, crm-events | `worker:9090`, `activity:9091`, `crm-events:9092` |

RPC-имена совпадают с development SAN. Для production DNS, которого нет в
`service-certs`, нужен сертификат среды. `service-certs` не выпускает
production SAN и не становится CA. TLS-проверку не ослаблять.

Readiness: standalone `/ready` — своя БД и нужные RPC. Core `/ready` не
требует продукта; отказ продукта виден на `/components/activity/ready` и как
typed `unavailable` на widget Activity, без пустой истории. Доказательства:
`TestComponentProcessesAndModeSwitch`, `TestComponentOSProcessFaults`,
MOD-02 unavailable tests — не переписывались.

## Fencing writers

Не запускать второй CRM Events scheduler против той же БД. Lease 30 с и
fencing защищают от **поздней** записи, не от двух живых владельцев.
Enrichment берёт свой lease, не `event_sources`. Core outbox lease —
отдельный; при переносе Events Core может копить `pending_delivery` и
доставить после появления получателя (уже проверено process-тестом).

## Ресурсы и измерения

После переноса измерить RTT, RPC/deadline, pool wait, throughput. Добавлять
CPU/RAM тому владельцу, у кого очередь/deadline, а не replicas «всем».
Исходящий бюджет amoCRM остаётся у одного Gateway. Несколько экземпляров
продукта в этом этапе не вводятся.

## Локальная репетиция

```sh
# Конфиг overlay + restore Events на второй postgres. Не стартует api/worker.
sh deploy/activity/verify-transfer.sh
```

Compose-проект по умолчанию `amocrm-stage8-transfer-test`. Живой gRPC cutover
(`TRANSFER_LIVE=1`) скрипт отвергает: полный стек на этой машине не
переключался. Для ручного стенда:

```sh
docker compose -p amocrm-stage8-transfer-test \
  -f docker-compose.activity.yml \
  -f docker-compose.activity-transfer.yml \
  config
```

Не вызывать `up` без `-p ...-test`. Не направлять overlay на volume проекта
`amocrm-activity`.
