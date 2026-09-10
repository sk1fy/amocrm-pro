# REL-02: измерение чтения Activity / CRM Events

**Дополнение аудита 10.09:** первоначальные L-01 snapshot и L-03 оценки
сериализации не закрывали работу workers и реальный транспорт. Актуальные
проверки: [fixes/README.md](fixes/README.md), [критерии](fixes/criteria.md).
Ниже сохранены исходные измерения и их ограничения. Новый read benchmark
не копирует protobuf conversion; local Panel/EventCard измеряются сериями
с JSON-сериализацией. L-01/L-03 выполняются отдельными тестами
`TestStage6WorkersUnderSharedBudget` и `TestStage6TransportMeasure`.

Дата фиксации критериев: 10 сентября 2026 года, **до прогона**.
Задача плана: этап 6, REL-02. Открытый остаток итогового аудита: L-01, L-02, L-03.
Это локальное измерение на синтетическом объёме, **не** готовность production и **не** замена `TestComponentProcessesAndModeSwitch`.

Команды измерения (после записи критериев):

```sh
# только postgres+init, без make activity-test / activity-ci
docker-compose -p amocrm-stage6-rel02-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  up --detach --wait postgres
docker-compose -p amocrm-stage6-rel02-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  exec -T postgres sh < deploy/activity/init-tests.sh
# затем пакетный test с STAGE6_REL02_MEASURE=true на events_components_test
```

Opt-in полного объёма: `STAGE6_REL02_MEASURE=true` и `CRM_EVENTS_TEST_DATABASE_URL` на БД с именем `*_test`. Без флага `TestStage6ReadMeasure` выполняет короткий CI-smoke (десятки строк, EXPLAIN индекса, Query/GetEvent) и **не Skip** — иначе `activity-ci` падает на незаявленном SKIP.

## Профиль стенда (зафиксирован до прогона)

Наблюдение Docker Engine **до** старта проекта `amocrm-stage6-rel02-test`:

| Параметр | Значение |
| --- | --- |
| Docker NCPU | 2 |
| Docker MemTotal | 4 094 447 616 bytes (~4 GiB) |
| Docker Architecture | aarch64 |
| Docker Server | 29.5.2 |
| Compose | 5.1.4 |
| Образ измерения | `postgres:17-alpine`, лимит сервиса `mem_limit: 512m`, `cpus: 2.0` (из `docker-compose.activity.yml`) |
| Host (не стенд теста) | darwin/arm64, Go 1.26.5, 10 CPU / 24 GiB; CI-образ Go 1.25.12 linux/arm64 |

Это тот же класс стенда, что в итоговом аудите (Linux aarch64, 2 CPU, ~4 GiB RAM). На хосте одновременно работали runtime-проект `amocrm-activity` и посторонние Postgres; **их БД не трогаем**. Измерения только в `events_components_test`.

Если Docker RAM не хватит на профиль — профиль = FAIL/blocked с OOM/timeout. Молча уменьшать датасет, чтобы получить PASS, нельзя.

## Синтетические профили

Все события синтетические. Тексты — повторяющиеся ASCII, без персональных данных.

| ID | Смысл | Объём (намерение) |
| --- | --- | --- |
| `small_department` | Небольшой отдел | 8 сотрудников, 7 суток, ~40 событий/сотрудник/сутки ≈ 2 240 строк, компактный JSON |
| `many_employees` | Панель на пределе 100 сотрудников | 100 сотрудников, 7 суток, ~50 событий/сотрудник/сутки ≈ 35 000 строк |
| `dense_day` | Плотный календарный день | 40 сотрудников, 1 сутки, ~400 событий/сотрудник ≈ 16 000 строк в 24 ч |
| `large_notes` | Большие примечания у вложенного JSON-лимита | 200 событий, `value_before`/`value_after` ≈ 32 KiB каждое |
| `long_backfill` | Длительный backfill в горизонте retention | 16 сотрудников, 30 суток, ~30 событий/сотрудник/сутки ≈ 14 400 строк + queued backfill jobs |
| `l01_multi_install` | Несколько установок | 5 installation: current+backfill jobs разного возраста; mix pending / ready-refresh / `event_payload` / retry |
| `l03_bound` | Граница ответа 3 MiB | Некомпактная страница до и сверх `MaxResponseBytes`; карточка одного большого события |

L-02 EXPLAIN выполняется на `many_employees` и `dense_day` (реалистичный объём, где seq scan уже не «дешевле из-за крошечной таблицы»). `small_department` нужен как контроль, а не как основание для индекса.

## Пороги успеха/отказа (зафиксированы до прогона)

Пример impact-теста (`loadedP95<1s`, `P99<2s` для ping) **не** копируется слепо: ping — короткий HTTP admission, здесь — чтение истории/сводки/карточки. Пороги ниже — для read-path на этом стенде.

**Превышение порога = FAIL для разбора, а не повод поднять порог.**

### Задержки (wall time, P95/P99)

После 3 прогревочных вызовов — 25 измерений. P95 = ceil(0.95×N)-й элемент отсортированного ряда.

| Операция | P95 | P99 |
| --- | --- | --- |
| Owner `Query`: compact страница 100 + summaries + category counts + totals + hour timeline | 1.0 s | 2.0 s |
| Owner `Query`: non-compact страница малых payload (если ответ < 3 MiB) | 1.0 s | 2.0 s |
| Owner `GetEvent` + `LoadEventEnrichment` (карточка) | 200 ms | 500 ms |
| Owner `Claim` (collector job) | 100 ms | 300 ms |
| Owner `ClaimEnrichment` | 100 ms | 300 ms |
| Activity `Panel` (owner Query + presentation + JSON как HTTP body) | 1.2 s | 2.5 s |
| Activity `EventCard` | 300 ms | 700 ms |
| Отказ `resource_exhausted` на oversized Query | 1.0 s | 2.0 s |

### SQL EXPLAIN ANALYZE (execution time) и план

На профилях ≥ 10 000 строк:

| Запрос | execution time |
| --- | --- |
| history page (`ORDER BY created_at, event_id LIMIT`) | < 200 ms |
| summary `GROUP BY created_by` | < 400 ms |
| category counts | < 400 ms |
| totals | < 400 ms |
| timeline `width_bucket` | < 400 ms |
| `GetEvent` по PK | < 20 ms |
| `LoadEventEnrichment` | < 30 ms |
| claim `event_sources` / `event_jobs` | < 100 ms |
| enrichment claim | < 100 ms |

Индексное расследование (не автоматическое «добавить индекс»):

- `Seq Scan` / `Parallel Seq Scan` по `crm_events` на продуктовом запросе при ≥ 10 000 строк;
- либо shared read > 10 000 **или** Heap Fetches > 10 000 на странице из 100 строк.

Индекс предлагается **только** если это продуктовый путь (панель history/summary/timeline, GetEvent, claim) и план/буферы это подтверждают. «На всякий случай» — нет.

### Пул

При `MaxConns=8` (как в owner integration helper): P95 ожидания acquire < 100 ms. Исчерпание пула с очередью секунд — FAIL.

### Размеры RPC/HTTP

| Граница | Правило |
| --- | --- |
| Owner/Activity JSON | `serviceapi.MaxResponseBytes` = 3 MiB. Успешный ответ строго ≤ 3 MiB |
| gRPC frame | `servicerpc.MaxMessageSize` = 4 MiB. Proto payload успешного ответа < 4 MiB |
| Compact-журнал больших notes | JSON < 3 MiB (иначе FAIL контракта compact) |
| Non-compact страница, заведомо > 3 MiB | обязан вернуть `resource_exhausted`, не усечённое тело |
| Одна карточка ~32 KiB B/A | JSON < 3 MiB |

Выход за 3 MiB без `resource_exhausted` — FAIL. Сам отказ по лимиту — успех контракта, не повод менять лимит.

### amoCRM request volume

`Query` / `GetEvent` / `Status` / `Claim` / `ClaimEnrichment` не вызывают Gateway Events/Notes/Tasks/Pipelines/CustomFields/Entities. Любой такой вызов на read-path — FAIL.

`Activity.Panel` может один раз вызвать Gateway `Users` (справочник сотрудников); это не Events API. Счётчик Users записывается отдельно. Если инструментация невозможна — явно написать.

### L-01 (без отдельных порогов доли)

Фиксируются числа, не «зелёный процент»:

- доля refresh: claimable `ready AND source<>event_payload` / все claimable по предикату `enrichmentClaimable`;
- возраст очереди: `crm_events_oldest_job_age_seconds`, `crm_events_enrichment_oldest_age_seconds`;
- collector lag: `crm_events_max_lag_seconds` и `SyncStatus.LagSeconds` по нескольким installation;
- задержка карточки — пороги GetEvent/EventCard выше.

Если Claim/карточка укладываются в пороги при 5 установках, отдельный индекс/агрегат по L-01 не требуется, пока L-02 не покажет seq scan.

## Результаты прогона

Пороги выше **не пересматривались**. Прогон: `golang:1.25-alpine` linux/arm64, сеть `amocrm-stage6-rel02-test_default`, БД `events_components_test`. PostgreSQL 17.10, `shared_buffers=128MB`, `work_mem=4MB`. Opt-in test `TestStage6ReadMeasure` — PASS, 18.04 s, 25 samples после 3 warmup. Лог: [test.log](rel-02/test.log). Сводка: [summary.json](rel-02/summary.json). Стенд в контейнере: [stand.json](rel-02/stand.json).

Промежуточный FAIL первого прогона (17.56 s) был из-за слишком жёсткого правила «любой Seq Scan по `crm_events` при ≥10k строк = FAIL». Те же EXPLAIN показали execution 2–20 ms при shared hit, без disk read. Правило теста выровнено с критерием: Seq Scan страницы `history-compact` — FAIL (индекс `crm_events_time` обязан работать); Seq Scan агрегатов/фильтров — наблюдение, FAIL только при превышении execution. Пороги не поднимались. Второй прогон: 0 failures.

Ни один профиль не упёрся в RAM/OOM; датасеты не урезались.

### Наблюдаемые P95/P99 и размеры

Owner `Query` включает страницу + summaries + category counts + totals + timeline.

| Профиль | строк | Query compact P95 / P99 | Query full P95 / P99 | GetEvent P95 / P99 | HTTP panel | RPC panel | HTTP card |
| --- | ---: | --- | --- | --- | ---: | ---: | ---: |
| small_department | 2 240 | 10.9 / 11.0 ms | 8.3 / 8.4 ms | 0.20 / 0.21 ms | 47 403 | 22 691 | 596 |
| many_employees | 35 000 | 146 / 165 ms | 239 / 357 ms | 0.14 / 0.14 ms | 105 370 | 41 277 | 852 |
| dense_day | 16 000 | 70 / 82 ms | 61 / 62 ms | 0.14 / 0.14 ms | 68 949 | 29 634 | 596 |
| large_notes | 200 | 2.2 / 2.3 ms | — (non-compact отдельно в L-03) | 0.98 / 1.29 ms | 51 016 | 23 904 | 128 608 |
| long_backfill | 14 400 | 73 / 73 ms | 57 / 64 ms | 0.20 / 0.21 ms | 54 296 | 24 963 | 596 |

Claim (long_backfill): P95 687 µs, P99 1.7 ms. Claim 5 установок (l01): P95 525 µs, P99 1.4 ms. ClaimEnrichment (many_employees): P95 1.9 ms; (l01): P95 1.2 ms. Activity Panel many_employees: 117 ms. Все ниже порогов.

JSON compact many_employees: 76 216 байт. RPC QueryResult (локальная копия `toQueryResult`, unexported в `servicerpc`): 24 771 байт. Пул `MaxConns=8`, empty_acquire=2, суммарный acquire 6.8 ms.

Профили: [profiles/](rel-02/profiles/).

### Выводы EXPLAIN по запросам

Артефакты: [explain/](rel-02/explain/). Цифры — `many_employees` (35 000 строк), если не указано иначе.

| Запрос | План | Execution | Буферы |
| --- | --- | --- | --- |
| history compact LIMIT 101 | Index Scan `crm_events_time` | 0.08 ms | shared hit=7, read=0 |
| history authors (10 из 100) | Index Scan `crm_events_time` + Filter `created_by` | 0.16 ms | hit=42 |
| history types=`task_completed` | Index Scan `crm_events_time` + Filter | 0.14 ms | hit=46 |
| history unknown authors | Index Scan `crm_events_time` | 0.07 ms | hit=7 |
| history prefix `custom_field_` | Seq Scan `crm_events` (`left(event_type,…)` не sargable) | 4.7 ms | hit=1081, read=0 |
| history entity_type+ids | Seq Scan `crm_events` | 2.4 ms | hit=1081 |
| history category CASE | Seq Scan `crm_events` | 10.6 ms | hit=1081 |
| summary GROUP BY created_by | Seq Scan всего окна (35 000/35 000) | 18 ms | hit=1081 |
| category counts | Seq Scan окна | 16 ms | hit=1081 |
| totals | Seq Scan окна | 20 ms | hit=1081 |
| timeline width_bucket | Seq Scan окна | 10 ms | hit=1081 |
| GetEvent | Index Scan `crm_events_pkey` | 0.03 ms | hit=5 |
| LoadEventEnrichment | join links/objects | 0.06 ms | hit=5 |
| claim source (5 install, 20 jobs) | Seq Scan маленьких `event_jobs`/`event_sources` | 0.07 ms | hit=18 |
| enrichment claim | Index Scan `event_enrichment_claim` | 0.32 ms | hit=25 |

`crm_events_user_time` существует, но планировщик на полном 7-дневном окне предпочёл `crm_events_time` + Filter; для LIMIT 100 это 0.16 ms. Агрегаты читают все строки окна — Seq Scan здесь дешевле индекса. Prefix/entity/category Seq Scan при 35k укладывается в 11 ms без disk read; новый индекс не измерен как необходимость. Seq Scan `event_sources` (1–5 строк) — PK-таблица без отдельного вторичного индекса, не продуктовый объём.

### L-01 / L-02 / L-03

| ID | Статус | Доказательства |
| --- | --- | --- |
| L-01 | измерен | [l01.json](rel-02/l01.json), [profiles/l01_multi_install.json](rel-02/profiles/l01_multi_install.json), explain `l01_multi_install-claim-*.txt` |
| L-02 | измерен | [explain/](rel-02/explain/), профили many_employees / dense_day / long_backfill |
| L-03 | измерен | [l03.json](rel-02/l03.json), large_notes |

**L-01.** 5 installation. Очередь: 20 queued jobs, возраст 2040 s (синтетический `created_at`). Collector lag snapshot 2100 s; `SyncStatus.LagSeconds` = 420, 840, 1260, 1680, 2100. Enrichment: pending 56, ready 112, retry 56, unavailable 56; возраст 5880 s. Claimable 224, refreshable ready 56, доля refresh **0.25**, event_payload 56. Карточка many_employees P95 144 µs; Claim P95 525 µs; ClaimEnrichment P95 1.2 ms. Пороги не превышены — отдельный индекс/агрегат по L-01 не нужен.

**L-02.** См. таблицу EXPLAIN. Продуктовая страница и PK-карточка используют существующие индексы. Агрегаты/некоторые фильтры Seq Scan, но execution ≪ 200/400 ms.

**L-03.** Owner → JSON (3 MiB) → protobuf size (4 MiB, копия conversion) → Activity Panel/EventCard → `json.Marshal` как HTTP body (`activitybridge.respond`). Compact 100 больших notes: JSON 25 072, HTTP panel 48 338, RPC panel 22 910. Non-compact limit=100: `resource_exhausted` за 47 ms. Последний успешный non-compact limit=48, JSON 3 088 807 (< 3 145 728), RPC 3 079 303 (< 4 MiB). Limit≥49 превышает JSON-лимит. Карточка ~32 KiB B/A: JSON 64 191. Тихого усечения нет.

### Решение по индексам и агрегатам

**Изменение не требуется.** `crm_events_time` обслуживает журнальную страницу; `crm_events_pkey` — GetEvent; `event_enrichment_claim` — enrichment claim. Агрегаты окна и CASE/prefix/entity на 35k строк заканчиваются за ≤20 ms из shared buffers. Файл `rel-02-index-proposal.sql` не создавался. Redis, кеш, агрегатные таблицы и физическое разбиение БД не вводятся.

### amoCRM request volume

Инструментирован stub Gateway в том же test. На `Query` / `GetEvent` / `Claim` / `ClaimEnrichment` / `Status`: Events=Notes=Tasks=Pipelines=CustomFields=Entities=**0**. Activity `Panel`/`EventCard` вызывают только Gateway **Users** (1–2 раза на измерение размеров) — справочник, не Events API.

### Cleanup Compose

```sh
docker-compose -p amocrm-stage6-rel02-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  down --volumes --remove-orphans
```

Runtime-проект `amocrm-activity` не останавливался. Измерение не применялось к `amocrm_events`.

### Ограничения

- Синтетический объём на 2 CPU / 512 MiB Postgres, не пилот и не production SLO.
- Process impact test не заменяет L-01–L-03.
- Protobuf size считался локальной копией generated conversion (`toEvent`/`toQueryResult`/`toPanel` unexported); живой gRPC/mTLS не поднимался.
- Пакетный `go test ./internal/services/crmevents` на момент прогона не собирался целиком из-за параллельного WIP `stage6_collector_integration_test.go` (`g.notes`); REL-02 запускался списком файлов без него. Это не правка чужого файла.
- `make activity-ci` не запускался.
