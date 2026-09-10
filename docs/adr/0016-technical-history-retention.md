# ADR-0016: техническая история CRM Events и повторная доставка Core

Статус: принято, CORE-05 закрывает календарный горизонт redelivery. Дополняет
retention событий из миграции `000002` и [ADR-0014](0014-event-enrichment-contract.md).

## Решение

Горизонт `HistoryHorizon` = **7 суток от `created_at` команды** (Core
`activity_command_receipts.created_at`; CRM Events `event_inbox.created_at`).
Это календарный максимум, а не число попыток и не окно last-retry. Простой
исполнителя и `activity-control retry` его не продлевают. Ниже семи суток
выставить нельзя.

| Класс | Срок хранения | Причина |
| --- | --- | --- |
| `crm_events` | `retention_days` источника, 2..30 суток | Продуктовая история; действующий атомарный retention |
| Связанное enrichment | Пока есть ссылки от живых событий | Детали и названия нужны карточкам |
| Enrichment-сироты | Удаляются ограниченными пакетами после потери ссылок и окончания lease | Не нужны для живого журнала |
| `event_jobs` completed/failed | Не менее 7 суток после `updated_at`; живой lease защищён | Завершённая техническая история |
| `event_jobs` paused | До возобновления и завершения работы | Reauth/потеря прав/disable сохраняют диапазон и checkpoint |
| `event_operations` и `event_inbox` terminal | 7 суток от `created_at` квитанции; paused/accepted/running сохраняются | После Core-горизонта поздний replay не доставляется |
| `event_command_tombstones` | Ещё 7 суток после GC квитанции | Поздний Apply того же `command_id` не становится новым accept |
| `event_coverage` позади `retained_from` | Удаляются ограниченными пакетами при `window_to <= retained_from` | Эти окна вне гарантированной истории |
| Core `activity_command_outbox`/`activity_command_receipts` | Terminal (`accepted`/`failed`/`expired`) не раньше 7 суток от `created_at` | Тот же горизонт; pending/delivering сначала истекают |
| Core `jobs` и `job_attempts` | Terminal jobs 7 суток от `finished_at`; attempts каскадно | Bounded GC; jobs с живым outbound effect не удаляются |
| `audit_log` | 7 суток от `created_at` | Совпадает с окном операторского retry |
| `workflow_runs` terminal | 7 суток от `finished_at`, если нет оставшихся effects | История workflow не вечная |
| `outbound_effects` observed/no_effect/failed/expired | 7 суток от `updated_at` | Известный исход внешней мутации |
| `outbound_effects` prepared/applied/uncertain | Изменение не требуется | Исход внешней мутации неизвестен; reconcile — CORE-02 |
| `webhook_event_tombstones` | 90 суток от `last_seen_at`, не короче payload retention и 7 суток | Replay protection после удаления raw payload (ADR-0006) |
| Webhook `inbox_events`/`webhook_deliveries` | Прежняя политика 30 суток | Не сокращается |
| Activity owner `command_receipts` | Изменение не требуется | Другая БД; Core после горизонта не доставляет |

## Redelivery

Обычный worker, восстановление после простоя и операторский retry используют одно
правило: если `created_at` старше 7 суток, команда помечается `expired` /
`delivery_expired` и не отправляется. `RetryDelivery` возвращает typed
`conflict` и не сбрасывает attempts. Виджету `expired` показывается как
`failed`.

После GC terminal inbox/operations повтор того же `command_id` наталкивается на
tombstone и **не Apply**: настройки не откатываются. Новый `command_id`
принимается как обычно.

## Операторский CLI

Матрица (CORE-02 владеет `cmd/integrations`, здесь не менялся):

| Бинарь | Команды | Retry |
| --- | --- | --- |
| `cmd/integrations` | create/update/disable/enable/rotate-secret/set-service | Нет; необратимый эффект без reconciliation не повторяется |
| `cmd/activity-control` | `pilot-enable`/`pilot-disable`, `list`, `inspect`, `retry` | Единственный retry — прежний audited outbox retry с age guard |

`list`/`inspect` читают failed/expired доставки. Второго retry API нет.

## Очистка и метрики

`Retain` сохраняет прежнюю обработку одного источника за тик: `SKIP LOCKED`,
`RetentionBatch`, атомарное продвижение `retained_from`, удаление сирот.
Затем один глобальный пакет под advisory lock `39081476393` очищает завершённые
jobs, сироты enrichment, старое coverage, terminal inbox/operations и
просроченные command tombstones. Цикла до пустых таблиц нет.

Core `maintenance.Cleanup` под lock `6584483612447211903` добавляет bounded GC
jobs/attempts, audit, tombstones, terminal workflow/effects и Core receipts,
не удаляя webhook payloads раньше действующей политики и не трогая uncertain
effects.

Счётчики processed/inserted/updated/deduplicated копятся в `event_sources` и
переживают удаление jobs. Gauges исключают completed/failed, но включают paused
как ожидающую восстановления работу. Неизвестные статусы нормализуются в other.

## Выпуск

Owner-миграция `000007_technical_history` нужна до обновления CRM Events из-за
колонок `event_sources.events_*`. `000008_command_identity_horizon` включает
tombstones и GC квитанций. Core `000014_core_redelivery_horizon` добавляет
статус `expired` и индексы очистки. Down не возвращает удалённые строки.
