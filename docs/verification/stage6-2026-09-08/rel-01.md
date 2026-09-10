# REL-01. Сбор после расширения модели (enrichment)

Дата: 10 сентября 2026. Задача этапа 6 плана `docs/plans/2026-09-08-activity-backend-development.md`. Контракт: [ADR-0014](../../adr/0014-event-enrichment-contract.md).

Это проверка существующих гарантий коллектора на фоне sidecar-обогащения. Коллектор не переписывался, второй collector не создавался.

## Что уже было

Owner-тесты коллектора в `internal/services/crmevents/`:

| Файл | Что закрывает |
| --- | --- |
| `store_integration_test.go` | restart/дедуп, fencing/disable, нестабильная пагинация, policy/reauth, backfill vs current, атомарность страницы, 429/сеть, inbox Conflict |
| `recovery_integration_test.go` | долгий простой, checkpoint страницы, island backfill не закрывает gap |
| `audit_integration_test.go` | атомарный retention, snapshot-чтение |
| `enrichment_recovery_integration_test.go` | retry recovery, atomic claim, partial batch, reauth wait объекта |
| `enrichment_integration_test.go` | enqueue при SavePage, hash GetEvent, 404 не валит source, Notes error не останавливает сбор |
| `enrichment_test.go` | канонический hash без sidecar, mapping ошибок, mock FailEnrichment не зовёт Fail() |
| `worker.go` | `RunOnce`, затем `EnrichOnce` только если Claim не дал коллекторской работы |

Пробел REL-01: те же гарантии не были сведены с очередью enrichment (lease источника vs object lease, покрытие окна, приоритет нескольких установок, snapshot настроек).

## Что добавлено

Все новые сценарии — в одном файле, тот же пакет `crmevents`, хелперы `setup` / `accepted` / `runPages` / `testStore` / `seedRecoveryObject` / `drainEnrichment` переиспользованы.

Файл: `internal/services/crmevents/stage6_collector_integration_test.go`.

## Таблица новых Test*

| Тест | Пункт REL-01 |
| --- | --- |
| `TestCollectorGuaranteesHoldWithQueuedEnrichment` | 1. restart, crash after write, fencing, replay страницы, late event, нестабильная пагинация **при** queued/saved enrichment. Sidecar не меняет `content_hash`. Enrichment не занимает `event_sources.lease_token`. Устаревший enrichment-writer не перезаписывает более новый объект |
| `TestCollectorAndEnrichmentFailureIsolation` | 2. исходный сбор и enrichment при 429, 5xx, timeout, permission, reauth_required. `Fail()` коллектора сохраняет семантику source; `FailEnrichment()` не ставит source в `failed`/`reauth_required` и не двигает `continuous_to` / `event_coverage` |
| `TestEnrichmentFailureDoesNotBlockOrAdvanceCoverage` | 3. события окна пишутся первыми; retry enrichment не блокирует досбор окна и сам не вызывает `cover()`; успешный догон enrichment не двигает `VerifiedThrough` |
| `TestCurrentCollectionPriorityOverBackfillAndEnrichment` | 4. две установки: `Claim` берёт current A раньше backfill B; `ClaimEnrichment` не берёт объекты A, пока жив source lease A, и не трогает `event_sources.lease_token`. Когда оба collector lease живы — enrichment не допускается. После отсутствия коллекторской работы `ClaimEnrichment` снова берёт объекты (admission, не цикл `Run`) |
| `TestSettingsSnapshotOnQueuedAndRunningJobs` | 5. replay Configure/sync с другим payload → Conflict; смена initial/retention не переписывает окно queued/running job и не восстанавливает удалённые события |

## Snapshot/revision семантика настроек (код + тесты)

Отдельного revision protocol нет и не вводилось. Наблюдаемое поведение `Postgres.Apply` (`postgres.go`) и `normalizeCommand` (`service.go`):

1. **Идентичность команды.** Ключ inbox — `(installation_id, command_id)` плюс SHA-256 нормализованного payload (поле `Auth` в hash не входит). `InitialDays=0` / `RetentionDays=0` заполняются до hash как 2 и 7. Повтор того же `command_id` с тем же смыслом возвращает прежнюю operation. Тот же `command_id` с другим `InitialDays`/`RetentionDays`/`Kind`/диапазоном — `Conflict`. Это и есть защита от отката «старой Configure» с другим телом.

2. **Полный снимок, не patch.** Каждый новый `command_id` вида enable/sync/backfill сразу пишет `event_sources.initial_hours` и `retention_days` из команды. Частичного обновления одного поля нет.

3. **Attach к живому current job.** Если есть current job в `queued`/`running`/`retry`/`paused`, новый sync **присоединяет** operation к этому job (`event_operation_jobs`) и **не** пересчитывает `window_from` / `window_to` / `target_to` / page / scan_pass. Курсор страницы сохраняется. Очередь не «откатывается» к предыдущему snapshot окна: снимок работы — строка `event_jobs`.

4. **Source state при attach.** Apply выставляет `event_sources.state='pending'` и чистит `error_code`, но **не** увеличивает `lease_token` и не сбрасывает `lease_until`. Текущий writer с валидным claim по-прежнему может `SavePage`.

5. **InitialDays не перематывает покрытое.** Пока job жив, смена `InitialDays` меняет только `event_sources.initial_hours`. Если `continuous_to` уже есть и живого current job нет, новый job стартует с `continuous_to - overlap`, а не с `now - InitialDays`. Явный более старый диапазон — команда `backfill`.

6. **RetentionDays.** Новое значение — cutoff для `Retain`. Уже удалённые события не восстанавливаются. `continuous_to` / `VerifiedThrough` retention не перематывает.

7. **disable → enable/sync.** disable паузит jobs и fencing-ит source lease (`lease_token+1`, `lease_until=NULL`). Следующий sync снимает pause (страница сбрасывается в 1 только при `pagination_unstable` / `page_limit_exceeded`) и снова attach-ит; окно из нового `InitialDays` не пересобирается.

Проверено тестом `TestSettingsSnapshotOnQueuedAndRunningJobs`.

## BLOCKED-FOR-COORDINATOR

Нет. Продакшен-файлы не менялись. Новые тесты написаны на корректное поведение `Claim` / `ClaimEnrichment` / `Fail` / `FailEnrichment` / `Apply` как в текущем коде.

## Команды

```sh
gofmt -w internal/services/crmevents/stage6_collector_integration_test.go
go test -c -o /tmp/crmevents.test ./internal/services/crmevents
go test -list 'TestCollectorGuaranteesHoldWithQueuedEnrichment|TestCollectorAndEnrichmentFailureIsolation|TestEnrichmentFailureDoesNotBlockOrAdvanceCoverage|TestCurrentCollectionPriorityOverBackfillAndEnrichment|TestSettingsSnapshotOnQueuedAndRunningJobs' ./internal/services/crmevents
```

Хост-компиляция: exit 0. Лог: [rel-01/compile.txt](rel-01/compile.txt).

## Что не проверялось

- Прогон integration-тестов против Docker Postgres (`CRM_EVENTS_TEST_DATABASE_URL`).
- `make activity-ci` / `make activity-test` / docker-compose (владение координатора).
- Нагрузка REL-02, retention технической истории REL-03, живой amoCRM.
