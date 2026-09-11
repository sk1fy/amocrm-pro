# Этап 8: Backend Tester 2

Дата: 11 сентября 2026. Постоянный тестер:
`/Users/nikpeskov/Projects/sub-projects/amocrm-pro-service-2`.
Backend HEAD при сверке: `d7cb8c7a8b988df8401765581eb8fd7bef86f7db`.

Это локальный отчёт приёмки тестера, не production deploy и не живой QA-02.
Клиентский код виджета не менялся; ZIP не пересобирался. Версия не поднималась.

## Diff: source → vendor → generated AMD

Синхронизация не требовалась. Исходник адаптера
`examples/activity-v0` совпадает с vendor; AMD/CSS созданы штатным
`npm run activity-assets` (`--check` PASS). Три копии не правились независимо.

| Файл | SHA-256 | Результат |
| --- | --- | --- |
| `examples/activity-v0/panel.mjs` | `b00162a54c892a3f193466dbf84b27686ddb361ff5fa92e71cdb3345ee37c41b` | эталон |
| `vendor/activity-v0/panel.mjs` | тот же | байтово равен source |
| `widget/lib/activity-panel.js` | `ebf78d11bed0e3ea5b7f1716de6a83be46b5358ec28350ee90aa39ba1206b5ff` | AMD-обёртка из vendor (662 строки source → 668 AMD) |
| `examples/activity-v0/panel.css` | `e0e01401fa1d079178d2b78f2becb8abdbbb6592d9df8a1ef16c7eae28779a31` | эталон |
| `vendor/activity-v0/panel.css` | тот же | байтово равен source |
| `widget/lib/activity-panel.css` | тот же | байтово равен vendor/source |

AMD отличается только генерацией: заголовок `define([])`, `'use strict'`,
`export function` → `function`, финальный `return { observationLabel, … }`.
Содержимое адаптера совпадает. UI журнала/карточки/фильтров не переписывался.

Исправлений в `examples/activity-v0` для тестера не потребовалось.

## Версия и ZIP

- package/manifest: **0.5.2** (совпадают; не поднимались)
- `dist/widget.zip` SHA-256:
  `da6072eb003afa8ee1889c9e62b75cac358810f7288f7267d5fb00da87f884b7`
  (тот же артефакт финального аудита 08.09.2026)
- `npm run pack` не запускался: в `widget/` нет клиентских изменений

README тестера обновлён: было указано **0.4.0** при фактической 0.5.2.

## `npm run check`

```text
Widget structure is valid.
ℹ tests 45
ℹ pass 45
ℹ fail 0
ℹ skipped 0
```

Было 37 PASS (финальный аудит). Добавлены 8 Node-проверок контракта этапа 8
без второй реализации UI-01–03.

Сохранены прежние сценарии: bootstrap/ping/идемпотентность/jobs, lead-status/workflow,
Activity panel/sync/backfill/settings/operations, диагностика, postMessage-мост.

Новые проверки:

- allowlist карточки `GET /api/v1/widget/activity/events/{id}` и panel-query с
  `from`/`to`/`compact`/`group_id`/`user_ids`/`categories`
- фильтры vendored `panelQuery` и смонтированного журнала; `actor_id` не уходит
- compact-страница запрашивает карточку один раз; состояния enrichment
  ready/pending/unavailable на карточке
- короткий coverage/freshness vs свёрнутая диагностика
- bounded poll операции 12×5 с; `destroy` снимает таймер
- origin/source моста, отсутствие JWT в postMessage, cleanup после destroy
- виджет не содержит хостов product-сервисов; запросы только на публичный Core
  origin из `backend_url`

## Размещение Activity / CRM Events

Код виджета не зависит от размещения модулей Activity и CRM Events.
Единственный backend origin — HTTPS Core из настройки `backend_url`.
Адаптер ходит на относительные `/api/v1/widget/activity/...`.
Перенос product-процессов на другой сервер не требует смены виджета, пока
публичный Core origin тот же.

## QA-02 после установки

Локальные Node-проверки не заменяют SDK и аккаунт amoCRM. После загрузки
текущего ZIP **0.5.2** на выбранном аккаунте ещё нужно:

1. Реальные типы событий по матрице BASE-02: CRM → сборщик → карточка виджета.
2. Сверка полноты на том же аккаунте, периоде, правах и фильтрах.
3. Пользователь без событий, новый сотрудник, неизвестный автор, несколько отделов.
4. Закрыть виджет, сделать CRM-действие, открыть снова: сбор продолжался.
5. Живые JWT/CORS/CSP SDK, загрузка assets, пагинация, reauth, отключение pilot.
6. Записать commit, миграции, условия и обезличенные результаты; без реальных payload.

Успешная живая проверка прежней версии уже сообщалась; её не считать
отсутствующей. Старый установленный ZIP не проверяет новые клиентские сценарии
только если клиент сменился — в этой работе клиентский ZIP тот же 0.5.2.
Живой прогон 0.5.2 в аккаунте этой работой не выполнялся.
