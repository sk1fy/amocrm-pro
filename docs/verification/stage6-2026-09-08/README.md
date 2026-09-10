# Этап 6: надёжность, объём данных и совместимость

База: `dac12af8e1b8ef16fbac5e137c7238eaa0db335d` плюс изменения этапа 6.
Дата: 10 сентября 2026 года. Локальная реализация и Docker-gate, не production deploy.

**Актуальное состояние после CORE-05:** [REL-03](rel-03.md) и
[окончательные проверки этапа 7](../stage7-2026-09-08/fixes/README.md).
Terminal inbox/operations очищаются с tombstones после семисуточного Core
redelivery horizon. Paused jobs и Activity receipts сохраняются; их бюджет
остаётся предметом OPS-01/02. [Отчёт исправлений этапа 6](fixes/README.md)
ниже отражает историческое состояние до CORE-05. L-01 дополнен работающими
workers, L-03 — реальными HTTP/mTLS-соединениями.

**Исторический результат до повторного аудита:** независимый ревьюер — REL-01/02/03 pass, MOD-02 conditional
(публичный `GET /events/{id}` при падении Activity по-прежнему отдаёт owner-конверт
без `view` — существующий fallback `activitybridge`). Первый `activity-ci` упал на
двух тестах; исправлено. Итог: 205 верхнеуровневых Go PASS с `-race`, 3 штатных
helper SKIP, UI 25 PASS / 0 SKIP. [Ревью](review.md), [правки CI](fixes.md).

Новая owner-миграция: `000007_technical_history`. Продуктовый retention событий
2..30 дней не менялся. Redis, агрегатные таблицы и новые индексы чтения не
вводились. Исходные L-01 snapshot и L-03 оценки сериализации дополнены повторным
прогоном в `fixes/`.

## Что изменилось на момент этапа 6 (до CORE-05)

| Задача | Результат |
| --- | --- |
| REL-01 | Новые owner-тесты sidecar-обогащения: restart/crash/fencing/replay/late/unstable; 429/5xx/timeout/permission/reauth; ошибка enrichment не двигает coverage; current A раньше backfill B; snapshot настроек (Conflict на другой payload, attach без перемотки окна) |
| REL-02 | Пороги записаны до прогона. 35k строк / 100 сотрудников: compact Query P95 146 ms, GetEvent P95 144 µs, HTTP compact 48 KiB, non-compact 100 notes → `resource_exhausted` за 47 ms. EXPLAIN страницы — Index Scan `crm_events_time`. Seq Scan агрегатов 10–20 ms. Индексы/Redis/агрегаты не добавлялись |
| REL-03 | Bounded GC completed/failed jobs, orphan enrichment и coverage. Jobs ≥7 суток; paused, inbox и operations сохраняются. Ограничение хранения квитанций ожидает CORE-05 |
| MOD-02 | Embedded/mTLS parity compact/карточка/фильтры/ошибки; replay Configure после потери RPC; Panel 503 при падении Activity; старый peer fail-closed. Process ping/lead-status уже в `TestComponentProcessesAndModeSwitch` (прогнан в этом gate) |

Контракт хранения: [ADR-0016](../../adr/0016-technical-history-retention.md).
Измерения: [rel-02.md](rel-02.md). Collector: [rel-01.md](rel-01.md).
Retention: [rel-03.md](rel-03.md). Parity: [mod-02.md](mod-02.md).

## Проверки и границы доказательств

Исторический gate до исправлений аудита:

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| `make ACTIVITY_TEST_PROJECT=amocrm-stage6-test activity-ci` | exit 0; 205 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`; UI 25 PASS / 0 SKIP | [Go log](activity-go.txt), [UI log](activity-ui.txt), [checks.json](checks.json) |
| REL-02 полный объём | отдельный compose `amocrm-stage6-rel02-test`, `STAGE6_REL02_MEASURE=true`; не production load | [rel-02/](rel-02/) |

Compose-проект `amocrm-stage6-test`, БД `*_test`. Runtime-проект `amocrm-activity` не менялся.

Ключевые сценарии:

- enrichment не занимает `event_sources.lease_token` и не меняет `content_hash`;
- FailEnrichment не ставит source в `failed`/`reauth_required` и не двигает `VerifiedThrough`;
- current приоритетнее backfill; enrichment не берётся при живом collector lease;
- replay после GC jobs — та же operation и сохранённый hash; inbox не удаляется;
- `retention_days=2` не удаляет 3-дневную квитанцию;
- gauges не считают `completed`/`failed`; counters не сбрасываются при GC jobs;
- compact/GetEvent/categories/group_id совпадают local и mTLS;
- HTTP `/panel` при остановленном Activity — 503 `unavailable`, не пустая история.

## Что не выполнялось

- Живой amoCRM, OAuth SDK, production migrate `000007`
- CORE-05: максимальный срок redelivery и согласованная очистка command identity
- Redis, новые агрегатные таблицы, новые индексы чтения
- Смена существующего HTTP fallback карточки на 503 (ревью: suggestion, не баг этапа)
- Нагрузочная готовность production: измерения на стенде 2 CPU / ~4 GiB

## Команды воспроизведения

```sh
make ACTIVITY_TEST_PROJECT=amocrm-stage6-test activity-ci
```

Полный REL-02 (opt-in, тяжёлый):

```sh
# см. rel-02.md; STAGE6_REL02_MEASURE=true на events_components_test
```

Перед сервером: применить owner-миграцию `000007` согласованно с бинарём CRM Events
(колонки `event_sources.events_*`). Не включать GC inbox/operations до протокола
CORE-05. Down `000007` не возвращает удалённые строки.
