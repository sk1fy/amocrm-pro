# Исправления этапа 6 после первого CI

Первый `make ACTIVITY_TEST_PROJECT=amocrm-stage6-test activity-ci` упал на
`internal/services/crmevents` (exit 2). Остальные пакеты и UI не запускались.

| Тест | Причина | Исправление |
| --- | --- | --- |
| `TestCurrentCollectionPriorityOverBackfillAndEnrichment` | `runPages` заявляет jobs глобально. После `accepted(A)` он съедал текущее окно A, а у B оставался current pass 2, который Claim брал раньше backfill | Сначала завершить current B (`accepted`+`runPages`), повесить backfill, затем поставить current A |
| `TestStage6ReadMeasure` (CI smoke) | 40 строк: планировщик честно выбирает Seq Scan; smoke требовал Index Scan | Smoke проверяет наличие `crm_events_time` / `crm_events_user_time` и Query/GetEvent, не план на крошечной таблице. Полный EXPLAIN остаётся за `STAGE6_REL02_MEASURE=true` |

Повторный gate: exit 0, 205 PASS / 3 SKIP / 0 FAIL, UI 25 PASS.
Независимый ревьюер не требовал этих правок (он не гонял Docker).
