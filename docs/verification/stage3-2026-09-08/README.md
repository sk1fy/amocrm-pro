# Этап 3: обогащение событий и справочники

База: `8c36827ef041561d2b2d734e5c686f36869812f8` плюс сохранённые изменения этапов 1–2.
Дата: 8 сентября 2026 года. Это локальный отчёт реализации, не production deploy.

После общего аудита дополнительно исправлены терминальные временные ошибки,
отказ здоровых элементов батча и гонка claims одной установки. Добавлена
миграция `000006_enrichment_retry_recovery` и additive invalid_ids в Gateway.
Актуальный общий gate: 182 Go PASS с `-race`, UI 21 PASS.
[Отчёт и условия обновления](../audit-fixes-2026-09-08/README.md).

После ревью исправлены TTL, смешивание исторических payload и потеря больших
справочников. Итоговый повторный gate: 158 Go PASS с `-race`, 3 штатных helper
SKIP, UI 5 PASS / 0 SKIP. Добавлена миграция `000005_enrichment_refresh`.
Актуальные изменения, rollout и доказательства — [исправления ревью](fixes.md).
Ниже сохранён первоначальный результат этапа до этих исправлений.

## Что изменилось

| Задача | Результат |
| --- | --- |
| ENR-01 | Ограниченные Gateway RPC `Notes`, `Tasks`, `Pipelines`, `CustomFields`, `Entities`. System grants и mTLS только для CRM Events. Activity по-прежнему вызывает только `Users`. Общий amoCRM бюджет и `MapUpstreamError` без hop-by-hop role check |
| ENR-02 | Owner-таблица `event_enrichment_objects` + связи. Сначала пишется событие, затем ставится дедуплицированная работа. `RunOnce` остаётся коллектором; `EnrichOnce` берёт объекты со своим lease, не занимая `event_sources`. Состояния `pending/ready/unavailable/retry/error` и reason codes. Отказ Notes не останавливает сбор |
| ENR-03 | Имена сущностей, воронок/этапов и полей кешируются как current catalog. ID сохраняются. Недоступный объект — тип/ID, не пустая карточка. Аватары не добавляются. Авторы вне directory остаются в истории и доступны через GetEvent |
| ENR-04 | Факты задач/звонков/заметок/вложений из payload или текущего чтения. `created_by` и `call_responsible` раздельно; `src` и `source` не смешиваются; запись не скачивается; текст чата не выдумывается |

Точный контракт: [ADR-0014](../../adr/0014-event-enrichment-contract.md).
Матрица: [спецификация](../../specs/activity-data-contract.md).
Fixture samples остаются отдельными текущими ответами, не историей B/A.

## Использование API

Панель не изменилась и не ходит в amoCRM за Notes. Подробности enrichment читаются с уже существующего detail:

```text
GET /api/v1/widget/activity/events/{eventID}
```

В ответе события могут появиться необязательные массивы `enrichment` и `names`.
Это текущее состояние на `fetched_at`, не замена `value_before`/`value_after`.
Список панели эти тела не включает. Compact-null по-прежнему означает проекцию, не `unavailable`.

Новые внутренние Gateway RPC недоступны Activity и виджету. Старый Gateway без них не ломает сбор: enrichment остаётся `unavailable`.

## Проверки и границы доказательств

Итоговый gate:

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| `make ... activity-ci` | exit 0; 153 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`; UI 5 PASS | [Go log](activity-go.txt), [UI log](activity-ui.txt) |

Compose-проект `amocrm-stage3-test`, БД `*_test`. Runtime-проект не менялся.
Первый прогон упал на сравнении GetEvent sidecar с историческим конвертом; исправлено через `HistoricalEvent`.

Ключевые сценарии:

- allowlist/батч/лимиты Notes, Tasks, Pipelines, Custom Fields, Entities;
- mTLS: только `crm-events` вызывает новые Gateway методы; панель не получает эти grants;
- collector Issue system `{gateway,notes|tasks|...}` без `GetUserAuthorization`;
- планирование fixture: reference note pending, embedded ready из payload, chat без Notes, task result без Notes на `task`, lead_status → pipeline catalog;
- факты: attribution unconfirmed, null link, missing duration, chat text не копируется;
- канонический hash события не меняется от sidecar;
- EnrichOnce + unavailable Gateway не переводит source в failed.

## Что не выполнялось

- Живой amoCRM, OAuth SDK, скачивание записей и Chat API
- Production migrate `000004`, ZIP виджета и UI карточек (этапы 4–5, 8)
- Аватары и второй directory
- Events `with` / `_embedded`

## Команды воспроизведения

```sh
go test -count=1 ./internal/integration/amocrm ./internal/gateway ./internal/corepolicy ./internal/servicerpc ./internal/services/crmevents
make COMPOSE=docker-compose ACTIVITY_TEST_PROJECT=amocrm-stage3-test activity-ci
```

Protobuf: `protoc 31.1`, `protoc-gen-go v1.36.8`, `protoc-gen-go-grpc v1.5.1`;
преобразования — `api/proto/generate-adapters.py`.
Новая schema применяется владельцем CRM Events до нового runtime.
Down 000004 удаляет enrichment без восстановления.
`CRM_EVENTS_ENRICHMENT=0` отключает worker (process tests).
