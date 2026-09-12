# Контракт панелей Activity v1

Дата: 2026-09-11. ADR: [0024](../adr/0024-activity-share-panels.md).
Этап 9, APP-01. Это принятый контракт для backend, прототипа и будущей
вкладки TeamOS.

Идентичности не смешивать: TeamOS `company.id` ≠ amoCRM `account_id` ≠
Core `installation_id` / `integration_id`.

## Mapping старых полей панели

Источник: TeamOS `ActivityPanelEditor` / `ssd.rkrs.ru` и Vue
`rkrs_activity_panel`. Timestamp-ID (`Date.now()`) не переносится.

| Старое поле / возможность | Решение v1 | Комментарий |
| --- | --- | --- |
| `panel_name` | **перенесено** → `name` | 1–120 символов |
| `employees` | **перенесено** → `employee_ids` | 1–100, положительные amoCRM user ID; сервер проверяет принадлежность directory installation |
| `work_time_from`, `work_time_to` | **перенесено** → `display_window.{from,to}` | `HH:MM` 24h, только отображение; не факт отработанного времени и не SQL-фильтр событий |
| timezone аккаунта | **перенесено** → `timezone` | с directory Gateway; зритель не задаёт |
| `id` = `Date.now()` | **неподдерживаемо** | серверный UUID `id` |
| `activePanelLink` / самостоятельная сборка URL | **заменено** | сервер возвращает `share_url` + одноразовый `view_key` |
| `pipelines` | **отложено** | поле отсутствует; неизвестное поле в JSON → `invalid_argument`; UI не имитирует фильтр |
| `operators`, планы, нормативы, «эффективность» | **отложено** | |
| `correspondence_seconds`, `tasks_seconds`, цветовые блоки звонков | **отложено** | не выдавать норматив за измерение |
| Telegram token/chat/notifications | **неподдерживаемо** в v1 | не попадает в viewer DTO |
| глобальный `tasks`, Imbox, срезы сделок | **неподдерживаемо** | остаётся у старого продукта |
| двухминутные интервалы таймлайна | **отложено** | корзины ADR-0015: `hour` / `day` / `auto` |
| `{panels,tasks}` целиком | **неподдерживаемо** | одна панель = один PATCH |

## Идентификаторы и ссылка

- `id` — UUID панели, публичен в management API.
- `view_key` — секрет ссылки, 32 байта, unpadded base64url (~43 символа).
- `share_url` — `{ACTIVITY_APP_PUBLIC_ORIGIN}/#/p/{view_key}`. Origin задаёт
  конфигурация, не клиент.
- Сырой ключ виден только в ответах create и rotate. Список панелей и GET
  одной панели возвращают `share_url_issued: true/false` без ключа.
- Прототип открывает `/#/p/{view_key}` без `window.opener` и widget JWT.
- Зрителю не нужен вход в amoCRM, членство в аккаунте или проверка его роли.
  Доступ определяется действительным `view_key` включённой панели.
  Хостинг viewer-сайта не должен требовать дополнительного входа для этого пути.
  Отключение панели, отзыв ключа и серверная проверка состояния установки сохраняются.
  Управление панелями и административные действия виджета имеют отдельную авторизацию.

## Management API

Базовый origin — Core API. Вызов только с `ACTIVITY_MANAGEMENT_TOKEN`
(`Authorization: Bearer`) и заголовками:

- `X-Activity-Installation-Id` (UUID)
- `X-Activity-Integration-Id` (UUID)
- `Idempotency-Key` (UUID) для POST create и POST share-link
- `X-Request-ID` опционально

Неизвестный JSON-ключ тела — ошибка, не игнор.

| Метод | Путь | Результат |
| --- | --- | --- |
| GET | `/api/v1/activity/panels` | список management DTO без ключей |
| POST | `/api/v1/activity/panels` | 201, панель + `view_key` + `share_url` |
| GET | `/api/v1/activity/panels/{panelId}` | одна панель без ключа |
| PATCH | `/api/v1/activity/panels/{panelId}` | 200, CAS по `revision` |
| POST | `/api/v1/activity/panels/{panelId}/share-link` | 200, новый `view_key` + `share_url` |
| GET | `/api/v1/activity/employees` | directory installation, ≤100, группы |

### Management DTO

```json
{
  "id": "11111111-1111-1111-1111-111111111111",
  "name": "Смена А",
  "employee_ids": [7, 9],
  "display_window": {"from": "09:00", "to": "18:00"},
  "timezone": "Europe/Moscow",
  "enabled": true,
  "revision": 1,
  "updated_at": "2026-09-11T12:00:00Z",
  "share_url_issued": true
}
```

Create body: `name`, `employee_ids`, `display_window`. `timezone` сервер
заполняет из directory. `enabled` по умолчанию true.

PATCH body: обязателен `revision`; остальные поля optional. `enabled`
включает/отключает просмотр.

Create/rotate дополнительно: `view_key`, `share_url`. Сырой ключ есть
только в первом успешном ответе. Повтор того же Idempotency-Key и payload
возвращает ту же панель (`id`, revision, `share_url_issued=true`) без
повторной выдачи секрета: в БД хранится только SHA-256. Потерянный ключ
перевыпускают. Другой payload с тем же ключом — `409`, включая изменение `enabled`.
Отсутствующий `enabled` нормализуется в `true` при вычислении хеша.

Ошибки: `401` нет/неверный management token; `403` lifecycle/capability;
`404` панель чужого scope или отсутствует; `409` revision/idempotency;
`429` + `Retry-After`; `503` недоступная зависимость; `400` валидация.

## Viewer API

`Authorization: Bearer <view_key>`. Без installation headers.

| Метод | Путь | Назначение |
| --- | --- | --- |
| GET | `/api/v1/activity/view/panel` | настройки отображения этой панели |
| GET | `/api/v1/activity/view/timeline` | сводка, корзины, свежесть/покрытие |
| GET | `/api/v1/activity/view/employees/{employeeId}` | журнал сотрудника панели |
| GET | `/api/v1/activity/view/events/{eventId}` | карточка события панели |

Query timeline/employee: `from`, `to` (unix seconds, включительно, ≤31
сутки), `limit`, `cursor`, `buckets` (`hour`/`day`/`auto`). Категории —
как у ADR-0015, опционально. Сотрудник вне панели, чужой cursor, чужой
event ID → `404`.

### Viewer panel DTO

```json
{
  "name": "Смена А",
  "timezone": "Europe/Moscow",
  "display_window": {"from": "09:00", "to": "18:00"},
  "employees": [{"id": 7, "name": "Иванов", "group_id": 1, "group_name": "Отдел"}],
  "interpretation_version": 1
}
```

Нет `id` панели, installation/integration, revision, hash, view_key,
settings retention, management token.

Timeline использует существующие `totals`, `summaries`, `timeline`
buckets, `coverage`, `freshness`, `empty_reason` из ADR-0015. Итоги считает
backend за весь запрошенный период, не страница UI.

Employee page: `events` с `view` без обязательных details; details — на
карточке события. Compact journal допустим. Общий экран сохраняет `data.events` из timeline и
использует `data.next_cursor` для следующей страницы; итоги берёт из `totals`,
а не пересчитывает по этим событиям.

## CLI seed

Пока нет вкладки TeamOS:

```
activity-control panel-list INSTALLATION_UUID INTEGRATION_UUID
activity-control panel-create INSTALLATION_UUID INTEGRATION_UUID --name NAME --employees 7,9 --from 09:00 --to 18:00
activity-control panel-get INSTALLATION_UUID INTEGRATION_UUID PANEL_UUID
activity-control panel-patch INSTALLATION_UUID INTEGRATION_UUID PANEL_UUID --revision N [--name ...] [--employees ...] [--enabled true|false]
activity-control panel-rotate INSTALLATION_UUID INTEGRATION_UUID PANEL_UUID
activity-control panel-employees INSTALLATION_UUID INTEGRATION_UUID
```

CLI вызывает **HTTP management API** (env `API_BASE_URL`,
`ACTIVITY_MANAGEMENT_TOKEN`). Прямой SQL в Activity DB не является
приёмкой. Команды `list|inspect|retry|pilot-*` не меняются.

## Прототип

Репозиторий `/Users/nikpeskov/Projects/sub-projects/amocrm-pro-activity-site`.
Режим ссылки не требует моста виджета. Origin backend — конфигурация
сервера сайта / `ACTIVITY_API_ORIGIN`, не бизнес-логика карточек.

Одна команда локального запуска документируется в README сайта и в
отчёте APP-05. Fixture-режим, если есть, помечается отдельно от живого API.
