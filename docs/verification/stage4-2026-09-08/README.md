# Этап 4: контракты и представления Activity

База: `8c36827ef041561d2b2d734e5c686f36869812f8` плюс сохранённые изменения этапов 1–3.
Дата: 8 сентября 2026 года. Это локальный отчёт реализации, не production deploy.

**Повторное ревью:** исправлены определение неизвестных авторов при явном
выборе, корзины повторяющегося часа, выбор масштаба auto и сопоставление
названий по типу/ID. Актуальный gate: 174 Go PASS с `-race`, 3 штатных
helper SKIP, UI 5 PASS / 0 SKIP. Подробности и логи — [исправления](fixes.md).
Ниже сохранён предыдущий результат реализации до этих четырёх исправлений.

После ревью исправлены утечка авторов при пустом directory-списке, `group_id`
на старых peers, стороны before/after в карточке, лимит часовых корзин и
генератор pointer-`view`. Итоговый gate: 170 Go PASS с `-race`, 3 штатных
helper SKIP, UI 5 PASS / 0 SKIP. Owner-миграции этапа 4 нет.

## Что изменилось

| Задача | Результат |
| --- | --- |
| ACT-01 | Представления сотрудника, сводки, журнала и карточки на существующих `/panel` и `/events/{id}`. Новых URL нет. Лимиты 100/31д/3MiB сохранены |
| ACT-02 | 11 категорий BASE-03, исходный `type`, русские подписи, `custom_field_{id}`, неизвестный тип как `other`. `interpretation_version=1` |
| ACT-03 | Первое/последнее событие, категории, сущности, `task_completed` vs уникальные задачи — SQL за весь запрос. Нулевые сотрудники остаются. Неизвестные авторы на default panel |
| ACT-04 | `coverage` — выбранный период; `freshness` — лаг/ошибка сборщика. Проверенный день не становится stale из-за отставания. `empty_reason` различает проверенный ноль и непроверенный |
| ACT-05 | Корзины `hour`/`day`/`auto` в timezone аккаунта, coverage на неполных корзинах, максимум 48 часов. Не называется сменой |

Точный контракт: [ADR-0015](../../adr/0015-activity-presentation-contract.md).
Матрица: [спецификация](../../specs/activity-data-contract.md).

## Использование API

```text
GET /api/v1/widget/activity/panel?from=...&to=...&categories=tasks,calls&group_id=72001&buckets=auto
GET /api/v1/widget/activity/events/{eventID}
```

Панель добавляет `view` на событиях журнала, расширенные summaries/totals,
`freshness`, `empty_reason`, `interpretation_version`. Compact по-прежнему
опускает B/A. Карточка идёт через Activity, если порт доступен, иначе
исторический конверт CRM Events.

`read_version=3` у нового owner. Категории, unknown authors, buckets и
`group_id` требуют его; старый peer не игнорирует их молча. Курсоры без
новых полей сохраняют отпечаток v2.

## Проверки и границы доказательств

Итоговый gate:

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| `make ... activity-ci` | exit 0; 170 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`; UI 5 PASS / 0 SKIP | [Go log](activity-go.txt), [UI log](activity-ui.txt) |

Compose-проект `amocrm-stage4-test`, БД `*_test`. Runtime-проект не менялся.
Первый прогон упал: default panel стал включать неизвестных авторов (27→29
событий fixture) и fake owner без `read_version=3`. Исправлено в тестах и
в SQL неизвестных авторов.

Ключевые сценарии:

- классификация 29 fixture cases;
- проверенный период + лаг 900с остаётся `verified`/`lagging`;
- пустой проверенный период — `no_events`;
- неизвестные авторы на default panel, не при фильтре отдела;
- `user_ids` + `include_unknown_authors` без directory-списка не читает всех;
- категории и task metrics за весь запрос;
- hour buckets согласованы с totals в UTC;
- старый Activity/CRM Events отвергает новые options.

## Что не выполнялось

- Живой amoCRM, ZIP виджета и product UI (этап 5)
- Production deploy
- История членства отдела
- Агрегатные таблицы и Redis

## Команды воспроизведения

```sh
make ACTIVITY_TEST_PROJECT=amocrm-stage4-test activity-ci
```

Protobuf: `protoc 31.1`, `protoc-gen-go v1.36.8`, `protoc-gen-go-grpc v1.5.1`;
преобразования — `api/proto/generate-adapters.py` (pointer `Event.view`).
Owner-миграции этапа 4 нет.
