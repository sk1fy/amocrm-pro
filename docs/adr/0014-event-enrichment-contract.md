# ADR-0014: обогащение CRM Events и ограниченные справочники

Статус: принято для этапа 3. Дата: 2026-09-08.

Дополняет [ADR-0010](0010-activity-v0-service-ownership.md),
[ADR-0011](0011-activity-authorization-budget.md),
[ADR-0012](0012-separable-product-module-contract.md) и
[ADR-0013](0013-event-detail-read-contract.md).

## Решение о владельце

Исходные события остаются у CRM Events. Полученные примечания, задачи и
справочники тоже хранит CRM Events в собственной БД, отдельно от
`value_before`/`value_after`. Activity не вызывает amoCRM для этих чтений и не
создаёт вторую историю. Gateway выполняет только типизированные ограниченные
методы. Core по-прежнему владеет credentials, Issue и живой policy.

Текущее состояние объекта помечается как текущее: `current=true` и `fetched_at`.
Его нельзя выдавать за исторический снимок события. Поле, уже присутствующее в
B/A, не запрашивается повторно ради той же карточки. При GetEvent из payload
именно этого события создаётся enrichment с `source=event_payload`,
`current=false`, без копии в общем кеше примечаний и без внешнего запроса.
События с одним note ID сохраняют разные исторические тексты.

`ListEvents with` и `_embedded` не вводятся. Исторические события всё равно
требуют отдельных чтений имён; моделирование `_embedded` остаётся отдельным
решением. Аватары не добавляются.

## Ограниченные методы Gateway

Новые RPC, по одному действию на метод. Универсальный HTTP-прокси запрещён.

| RPC | Grant | Кто вызывает | Upstream | Лимиты |
| --- | --- | --- | --- | --- |
| `Notes` | `{gateway, notes}` | только CRM Events | `GET /api/v4/{entity}/notes` с `filter[id]` | 1 entity_type, до 50 ID, params ≤ 32 KiB |
| `Tasks` | `{gateway, tasks}` | только CRM Events | `GET /api/v4/tasks` с `filter[id]` | до 50 ID, text ≤ 32 KiB |
| `Pipelines` | `{gateway, pipelines}` | только CRM Events | `GET /api/v4/leads/pipelines` | один ответ, ≤ 512 KiB |
| `CustomFields` | `{gateway, custom_fields}` | только CRM Events | `GET /api/v4/{entity}/custom_fields` | страница 50, до 20 страниц |
| `Entities` | `{gateway, entities}` | только CRM Events | `GET /api/v4/{entity}?filter[id]` | 1 entity_type, до 50 ID |

Allowlist ресурсов: `leads`, `contacts`, `companies`, `customers`. Событийный
`lead` отображается в `leads`. Notes для `entity_type=task` не вызываются, пока
Task read не подтвердил родителя из allowlist. Chat/talk/file download нет.

Срок каждого вызова — 10 секунд. Используется существующий amoCRM клиент и
общий исходящий бюджет. Ошибки 401/403/404/429/5xx отображаются через
`MapUpstreamError`. Gateway не выполняет `GetUserAuthorization`. Кэш справочников
в Gateway — только отображение, 5 минут, с теми же ограничениями размера, что у
directory; авторизация проверяется живой Validate на каждый вызов.

Пользовательский токен панели по-прежнему содержит только
`{activity,panel}`, `{crm-events,read}`, `{gateway,users}`. Activity не получает
новые Gateway grants и mTLS на новые методы. Collector Events сохраняет только
`{gateway,events}` + `{crm-events,sync}`. Enrichment выдаёт отдельный system
token с одним нужным Gateway action.

## Асинхронный процесс

Сначала сохраняется исходное событие. В той же транзакции страницы ставится
дедуплицированная работа `(installation_id, object_kind, object_key)`. Одинаковые
примечание/задача/сущность схлопываются. Панель не ходит во внешний API на каждую
строку.

Обогащение **не занимает** `event_sources.lease_token` и не становится
`event_jobs.kind`. Отдельные строки `event_enrichment_objects` имеют собственный
lease. Сборщик остаётся приоритетным: worker берёт enrichment только когда
`Claim` вернул отсутствие коллекторской работы. Отказ Notes/справочника не
переводит source в `failed` и не двигает coverage.

Состояния объекта: `pending`, `ready`, `unavailable`, `retry`, `error`.
Ограниченные reason codes: `not_loaded`, `missing_in_source`, `not_found`,
`permission_denied`, `deleted`, `temporary`, `unsupported`, `invalid`.
Быстрый retry: до 5 попыток, затем медленный повтор через 10 минут. Негативный кеш:
`not_found` 15 минут, `permission_denied` 1 час. Готовый объект задачи/примечания
обновляется не чаще чем раз в 15 минут; справочники и имена сущностей — раз в 5 минут.
`ready` и `unavailable` с причинами `not_found`/`permission_denied` снова
доступны worker после `run_after`. Новый цикл сбрасывает счётчик попыток;
внутри `retry` счётчик сохраняется. После пяти быстрых временных ошибок
объект остаётся `retry/temporary`, повторяясь раз в 10 минут; счётчик
насыщается на MaxAttempts. Истечение времени повторов само по себе не
означает постоянную недоступность объекта. ReauthRequired ожидает восстановления
авторизации с интервалом один час. `unsupported` и `invalid` остаются терминальными.

`deleted` и `missing_in_source` зарезервированы: текущий объектный контракт
не выводит их из одного 404 или отсутствующего поля. Историческое действие
удаления остаётся в исходном событии; недоступный текущий объект — not_found.
Это ограничение различимости источника, а не подтверждение его удаления.

Admission на установку: один enrichment-claim, до 50 ключей одного kind,
один ограниченный Gateway-вызов kind/entity_type за claim. Внутри CustomFields
допускается до 20 страниц согласно лимиту метода, с общим deadline и внешним
бюджетом. Отдельная квота количества refresh-запросов на аккаунт не вводится;
её необходимость проверяется нагрузочными измерениями REL-02.

Admission claim сериализуется transaction advisory lock по installation_id;
после получения блокировки повторно проверяется активный object lease.
Блокировка живёт только до commit и не занимает collector source lease.
Финализация по-прежнему проверяет fencing token и срок object lease.

Notes/Tasks/Entities возвращают `invalid_ids` отдельно от корректных объектов.
Если params/text/name известного ID превышают лимит, его содержимое не
передаётся, а CRM Events сохраняет unavailable/invalid только для этого ID.
Неожиданный ID или структурно неполный ответ остаются ошибкой всего вызова.
Батч не делится на десятки внешних запросов.

## Чтение и факты

`Query`/панель не включают тела enrichment. `GetEvent` добавляет sidecar:

- `enrichment[]`: объект, состояние, reason, source, fetched_at, payload, current
- `names[]`: kind/id/name/state/current/entity_type

Лимит 32 KiB относится к исходным params/text, а не к целому справочнику.
CRM Events сохраняет объект с фактами целиком до 1 MiB; превышение даёт
`error/invalid` без payload, а не усечённый `ready`. Общий detail ограничен
существующим response limit 3 MiB.

Пустые sidecar не меняют канонический `content_hash` события. Compact-null B/A
не означает `unavailable`. Отсутствующий старый Gateway RPC даёт enrichment
`unavailable`, сбор продолжается.

ENR-04 извлекает факты в payload, не сливая события:

- задачи: текущий текст/срок/тип/результат помечены current; пустые B/A — валидный факт действия
- звонки: направление, duration, source как отдельный ключ от `src`, link без скачивания; `created_by` ≠ `call_responsible`
- сообщения: направление и доступный ID/текст; содержимое чата не выдумывается
- изменения: обе стороны B/A, включая удалённые значения
- вложения: uuid/имя; размер/MIME/URL только если источник их дал

Неизвестный автор и `created_by=0` сохраняются в истории. Fallback подписи —
представление Activity; GetEvent по ID уже доступен. Фильтр панели по текущему
directory не расширяется этим этапом.

Retention событий каскадно удаляет связи. Объекты без ссылок удаляются тем же
bounded Retain. Просроченный негативный кеш с действующими ссылками
перезагружается worker.

## Выпуск

1. Применить owner-миграции `000004_event_enrichment` и
   `000005_enrichment_refresh` владельцем CRM Events. 000005 расширяет индекс
   очереди, сбрасывает прежние общие `event_payload` и усечённые заглушки
   для повторной загрузки текущих данных. История берётся из `crm_events`.
2. Совместимо обновить Gateway, CRM Events, Activity и Core. Старый Gateway без
   новых RPC не ломает сбор: enrichment остаётся `unavailable`.
3. Down 000005 возвращает прежний индекс, но не восстанавливает сброшенный
   кеш. Down 000004 удаляет enrichment без восстановления содержимого.
4. Живой amoCRM, ZIP виджета и production rollout в локальную приёмку не входят.

После аудита этапов 1–5 добавить миграцию `000006_enrichment_retry_recovery`:
она возвращает прежние error/temporary в очередь. Down не восстанавливает
застрявшие состояния. Для invalid_ids обновить Gateway и CRM Events;
protobuf-поле additive. Старый owner безопасно увидит пропущенный объект как
not_found, а старый Gateway может продолжать отклонять батч целиком — точная
изоляция требует обновления обеих сторон. Публичный HTTP-контракт не меняется.
