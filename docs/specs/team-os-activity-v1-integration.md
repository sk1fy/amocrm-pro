# Инструкция для новой вкладки контроля активности в TeamOS

Дата: 11.09.2026. Связанный этап: [9. Прототип приложения и подготовка интеграции TeamOS](../plans/2026-09-08-activity-backend-development.md#stage-9), задача APP-04.

Контракт: [ADR-0024](../adr/0024-activity-share-panels.md), [activity-panel-v1.md](activity-panel-v1.md).

**Статус: документация для будущего задания TeamOS завершена. API панелей принят и зафиксирован; реализация вкладки этим документом не выполняется. Чеклист приёмки ниже — будущая работа исполнителя TeamOS и не считается выполненным от того, что инструкция написана.**

Это задание для отдельного агента в `/Users/nikpeskov/Projects/team-os`. Код TeamOS, маршруты, меню, компоненты, запросы и feature flags в рамках APP-04 не менять. Старые панели не мигрировать.

## Результат будущей работы

В TeamOS появится отдельная вкладка **«Контроль активности — новый»**. Она управляет панелями нового Go backend (Core management API). Пользователь создаёт панель, выбирает сотрудников и получает ссылку, по которой прототип Activity-сайта показывает таймлайны и детализацию.

Существующая вкладка `/activity-control` продолжает работать со старым `ssd.rkrs.ru`. Её настройки, ссылки, Telegram и task panel не мигрируются и не перезаписываются. Новый клиент не переключает base URL старого Rakurs-транспорта.

Просмотр по ссылке не требует сессии TeamOS, виджета amoCRM и операторского токена.

## Исходная реализация и точки расширения

| Существующий файл TeamOS | Что использовать |
| --- | --- |
| `src/pages/activity-control/ActivityControlPage.tsx` | Список, loading/error/empty, получение компании, действия карточки. Не копировать `setSettings({panels,tasks})` и сборку ссылки из `Date.now()` |
| `src/pages/activity-control/ActivityPanelEditor.tsx` | Выбор сотрудников по группам (`GroupedEmployeeSelector`), проверка формы, модалка. Не переносить pipelines, operators, Telegram, нормативы |
| `src/api/rakurs/activity.ts` | Mapping старых имён полей; не копировать legacy payload и нормализатор с fallback |
| `src/api/rakurs/client.ts` | Только изучение старого транспорта. `amo_account_id` в JSON не заменяет административный доступ |
| `src/api/client.ts`, `src/api/http.ts`, `src/stores/auth.ts` | Сессия TeamOS остаётся на origin TeamOS |
| `src/api/queryKeys.ts` | Добавить отдельное дерево `activityV2.*`, не расширять `queryKeys.activity` |
| `src/lib/permissions.ts`, `src/lib/permissions.test.ts` | Право `integrations` / `canManageIntegrations`; `moduleForPath('/activity-control')` уже ловит префикс, но тесты и prefetch должны знать новый путь явно |
| `src/App.tsx` | Новый lazy-route рядом со старым, тот же `RequireIntegrationAccess` |
| `src/components/layout/Sidebar.tsx` | Новый пункт меню, старый не заменять |
| `src/lib/routePrefetch.ts` | Отдельный loader для `/activity-control-v2` |

Целевой состав нового модуля (проверить отсутствие конфликта перед правкой):

| Назначение | Путь |
| --- | --- |
| Route | `/activity-control-v2` |
| Каталог страницы | `src/pages/activity-control-v2/` |
| API-клиент браузера | `src/api/activityV2.ts` |
| Query keys | `queryKeys.activityV2` |

Старый `/activity-control`, `src/pages/activity-control/`, `src/api/rakurs/activity.ts` и `queryKeys.activity` не трогать, кроме добавления соседнего пункта меню и соседнего route.

Переиспользовать UI-примитивы TeamOS (`PageHeader`, `EmptyState`, `ErrorState`, `Modal`, `Button`, `Switch`). Не подключать `VITE_RAKURS_ACTIVITY_API_URL` к новому клиенту.

## Идентичности и схема компания → installation

Не смешивать:

| Идентичность | Где живёт | Пример (синтетика) |
| --- | --- | --- |
| TeamOS `company.id` | сессия / `authApi.getCompany()` | `c0mpany00-0000-4000-8000-000000000001` |
| amoCRM `account_id` | поле компании `amoAccountId` | `"12345678"` |
| Core `installation_id` | заголовок `X-Activity-Installation-Id` | `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa` |
| Core `integration_id` | заголовок `X-Activity-Integration-Id` | `bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb` |

`company.amoAccountId` — **подсказка** для сопоставления, не доказательство прав и не ключ Core. Браузер не передаёт `amo_account_id` в Activity origin. Core не принимает TeamOS `company.id`.

Разрешение scope — обязанность **backend TeamOS** по сессии пользователя:

1. Взять текущую компанию из сессии, не из query string.
2. Найти связанные Core installations (по внутренней таблице/сервису сопоставления; `amoAccountId` только как hint).
3. Если сопоставления нет, `amoAccountId` пуст или installation не найдена — экран «Новый контроль активности не подключён» с конкретной причиной. Не подставлять тестовый account ID.
4. Если installation одна и сервер её подтвердил — можно выбрать её автоматически.
5. Если их несколько — **явный выбор** в UI. Не брать первую найденную. Выбранный `installation_id` с клиента backend перепроверяет: он должен принадлежать компании сессии.
6. `integration_id` подставляет сервер TeamOS, не форма в браузере.

Не переключаться молча на старый Rakurs API, если новый scope не решён.

## Доступ: кто кого вызывает

Предпочтительный и принятый путь (ADR-0024): **сессия TeamOS → доверенный server-to-server вызов Core management API**.

```
Браузер TeamOS
  Authorization: Bearer <TeamOS access token>     // только origin TeamOS
        ↓
Backend TeamOS
  проверяет сессию и роль owner/admin
  решает company → installation/integration
  Authorization: Bearer <ACTIVITY_MANAGEMENT_TOKEN>
  X-Activity-Installation-Id, X-Activity-Integration-Id
  Idempotency-Key на create/rotate
        ↓
Core API  /api/v1/activity/panels*  и  /api/v1/activity/employees
```

Правила:

- Браузер **не** отправляет Bearer TeamOS на origin Activity / Core.
- Браузер **не** получает `ACTIVITY_MANAGEMENT_TOKEN`. Секрет живёт только в конфигурации backend TeamOS (stand-in доверенного межсервисного вызова, не пользовательская сессия и не widget JWT).
- Management CORS у Core не нужен: браузер эти маршруты не вызывает.
- Прямой браузерный доступ к Core management API **не выбирать**. Если когда-нибудь понадобится, это отдельный ADR: audience, CORS, отзыв. Не копировать заголовки между сервисами автоматически.
- UI-права меню дополняют серверные проверки, но не заменяют их. Отдельный логин только ради Activity не создавать. Для первого управления — существующие `owner` / `admin` (`canManageIntegrations`).
- Kind `"operator"` выпускает Core для CLI и backend TeamOS. Вкладка не ходит в widget URL `/api/v1/widget/activity/*` и не использует viewer key для управления.

Пока backend TeamOS не умеет резолвить installation или нет management token на сервере — показать «не подключён» и причину. Не имитировать успех mock-данными.

## Ссылка просмотра

Сервер выдаёт `share_url` вида `{ACTIVITY_APP_PUBLIC_ORIGIN}/#/p/{view_key}`. Origin задаёт конфигурация Core, не клиент TeamOS и не `window.location`.

- `id` панели — UUID, не секрет, виден в management API.
- `view_key` — 32 криптостойких байта, unpadded base64url (~43 символа). Timestamp-ID старого клиента секретом не является.
- Сырой ключ и `share_url` (ключ внутри URL) возвращаются **только** в ответах create и rotate. Список и GET одной панели отдают `share_url_issued: true|false` без ключа.
- Прототип: репозиторий `/Users/nikpeskov/Projects/sub-projects/amocrm-pro-activity-site`. Открытие `/#/p/{view_key}` не требует сессии TeamOS, `window.opener`, widget JWT, файла токена и открытой вкладки amoCRM.
- Ключ просмотра не даёт settings/sync/backfill, список всех панелей, чужих сотрудников, management API и секреты.
- Неизвестный, отозванный и отключённый ключ на viewer API дают одинаковый `404`.
- Перевыпуск увеличивает `view_key_version`; прежняя ссылка недействительна на следующем чтении Activity.
- `enabled=false` сохраняет ряд и hash, просмотр отвечает `404`. Сбор CRM Events не зависит от `enabled` и не останавливается отключением или удалением панели.
- Ключи не писать в аналитику, логи, query keys, fixture, toasts с полным URL в отчётах и тесты. В репозитории — только синтетика.

Пока вкладки нет, тестовые панели создаёт CLI `activity-control panel-*` (HTTP management API, env `API_BASE_URL` + `ACTIVITY_MANAGEMENT_TOKEN`). Прямой SQL в Activity DB не является приёмкой.

## Экраны и действия

### Список панелей

Поля карточки: `name`, число `employee_ids`, `display_window` (`from`–`to`), `timezone`, `enabled`, `share_url_issued`, `updated_at`.

Действия:

| Действие | Как |
| --- | --- |
| Создать | POST; после 201 показать `share_url` один раз: скопировать / открыть |
| Изменить | GET одной панели → редактор с текущим `revision` |
| Открыть / скопировать ссылку | Только если в памяти ещё есть `share_url` этого сеанса (ответ create/rotate). Иначе объяснить: ключ повторно не читается; чтобы получить новую ссылку — перевыпуск, прежняя перестанет работать. Не перевыпускать молча ради копирования |
| Отключить / включить | PATCH `{ "revision", "enabled" }` |
| Перевыпустить ссылку | Подтверждение «прежняя ссылка перестанет работать» → POST share-link |

Пустой список, загрузка, ошибка запроса и «backend недоступен» показываются раздельно. Не подменять 503 пустым успешным списком.

Отключение, перевыпуск и изменение одной панели не удаляют историю CRM Events и не меняют другие панели и старый `/activity-control`.

Лимит: ≤50 панелей на installation. При лимите кнопка создания недоступна с объяснением.

### Создание и редактирование

Минимальные поля формы: название, непустой список сотрудников, окно отображения `HH:MM` 24h. Часовой пояс **не** редактируется: его заполняет сервер из directory Gateway на создании; в редакторе показать read-only.

ID новой панели выдаёт сервер. Не использовать `Date.now()` и не собирать секрет ссылки в клиенте.

Для PATCH обязателен полученный `revision`. Конфликт (`409`): сохранить локальный черновик, показать что данные изменил другой администратор, предложить загрузить актуальную версию и не затирать её своим PATCH. Не использовать прежний `setSettings({panels,tasks})`.

Сервер проверяет принадлежность каждого `employee_id` directory этой installation и все пределы. Справочник — `GET /api/v1/activity/employees` (через BFF), ≤100, с группами. Не подмешивать сотрудников другой компании и не выдумывать имена при недоступном directory.

### Ограничения первой версии (из mapping)

| Старое поле / возможность | Первая версия нового редактора |
| --- | --- |
| `panel_name` | `name`, 1–120 символов |
| `employees` | `employee_ids`, 1–100 положительных amoCRM user ID |
| `work_time_from`, `work_time_to` | `display_window.{from,to}`; только отображение, не факт отработанного времени и не SQL-фильтр событий |
| timezone аккаунта | `timezone` с сервера; зритель и форма его не подменяют |
| `id` = `Date.now()`, `activePanelLink` | серверный UUID и `share_url` из create/rotate |
| `pipelines` | **нет**. Поле не слать; неизвестный JSON-ключ → `invalid_argument`. UI не имитирует фильтр воронок |
| `operators`, планы, нормативы, «эффективность» | **нет** |
| `correspondence_seconds`, `tasks_seconds`, цветовые блоки звонков | **нет**; норматив не выдавать за измерение |
| Telegram token/chat/notifications | **нет**; не попадает в viewer DTO |
| глобальный `tasks`, Imbox, срезы сделок | остаются у старой вкладки |
| двухминутные интервалы таймлайна | **нет**; у зрителя корзины ADR-0015: `hour` / `day` / `auto` |
| `{panels,tasks}` целиком | **нет**; одна панель = один PATCH |

Период чтения у зрителя — как у widget query, ≤31 суток. Это ограничение прототипа, не поле редактора TeamOS.

## Management API (принятый контракт)

Базовый origin — Core API. Вызывает только backend TeamOS и CLI, не браузер и не ключ просмотра.

### Заголовки

| Заголовок | Обязателен | Значение |
| --- | --- | --- |
| `Authorization` | да | `Bearer <ACTIVITY_MANAGEMENT_TOKEN>` |
| `X-Activity-Installation-Id` | да | UUID installation |
| `X-Activity-Integration-Id` | да | UUID integration |
| `Idempotency-Key` | да для POST create и POST share-link | UUID; один ключ на один пользовательский intent |
| `X-Request-ID` | нет | UUID корреляции |
| `Content-Type` | да для тел | `application/json` |
| `Accept` | да | `application/json` |

Неизвестный JSON-ключ тела — ошибка, не игнор. `additionalProperties: false`.

Повтор того же `Idempotency-Key` и того же payload возвращает ту же панель (`id`, revision, `share_url_issued=true`) **без повторной выдачи** `view_key` / `share_url`: в БД хранится только SHA-256. Сырой ключ есть только в первом 201/200. Если клиент потерял ключ — `POST .../share-link` (прежняя ссылка перестанет работать). Другой payload с тем же Idempotency-Key — `409` `conflict`. При неопределённом исходе повторять **тот же** ключ и payload, не генерировать новый.

Синхронный CRUD: HTTP 200/201. Фиктивную очередь и HTTP 202 в UI не вводить.

### Маршруты

| Метод | Путь | Успех | Тело запроса | Тело ответа |
| --- | --- | --- | --- | --- |
| GET | `/api/v1/activity/panels` | 200 | нет | `{ "panels": [ ManagementPanel, ... ] }` без ключей |
| POST | `/api/v1/activity/panels` | 201 | `CreatePanel` | `ManagementPanel` + `view_key` + `share_url` |
| GET | `/api/v1/activity/panels/{panelId}` | 200 | нет | `ManagementPanel` без ключа |
| PATCH | `/api/v1/activity/panels/{panelId}` | 200 | `PatchPanel` (есть `revision`) | `ManagementPanel` без ключа |
| POST | `/api/v1/activity/panels/{panelId}/share-link` | 200 | пустой объект `{}` | `ManagementPanel` + новый `view_key` + `share_url` |
| GET | `/api/v1/activity/employees` | 200 | нет | `{ "users": [ {id,name,group_id,group_name}, ... ], "timezone": "Europe/Moscow" }`, ≤100 |

`panelId` — UUID. Чужой scope и отсутствующая панель — одинаковый `404`.

### DTO

`ManagementPanel`:

```json
{
  "id": "11111111-1111-1111-1111-111111111111",
  "name": "Смена А",
  "employee_ids": [7, 9],
  "display_window": { "from": "09:00", "to": "18:00" },
  "timezone": "Europe/Moscow",
  "enabled": true,
  "revision": 1,
  "updated_at": "2026-09-11T12:00:00Z",
  "share_url_issued": true
}
```

`CreatePanel`: только `name`, `employee_ids`, `display_window`. `timezone` сервер заполняет из directory. `enabled` по умолчанию `true`. Не слать `id`, `revision`, `view_key`, `share_url`.

`PatchPanel`: обязателен `revision` (целое, как получено). Остальные поля optional: `name`, `employee_ids`, `display_window`, `enabled`. Частичный PATCH не затирает непереданные поля. `enabled` включает/отключает просмотр.

Create/rotate дополнительно:

```json
{
  "view_key": "abcdefghijklmnopqrstuvwxyz0123456789ABCDE",
  "share_url": "https://activity.example.test/#/p/abcdefghijklmnopqrstuvwxyz0123456789ABCDE"
}
```

Значения выше — синтетика. Не копировать живые ключи в репозиторий.

`DirectoryEmployee`:

```json
{
  "id": 7,
  "name": "Иванов",
  "group_id": 1,
  "group_name": "Отдел"
}
```

### Пример вызова create (синтетика)

Запрос backend TeamOS → Core:

```http
POST /api/v1/activity/panels HTTP/1.1
Host: api.example.test
Authorization: Bearer management-token-example-not-a-secret
X-Activity-Installation-Id: aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
X-Activity-Integration-Id: bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
Idempotency-Key: 33333333-3333-4333-8333-333333333333
X-Request-ID: 44444444-4444-4444-8444-444444444444
Content-Type: application/json
Accept: application/json

{
  "name": "Смена А",
  "employee_ids": [7, 9],
  "display_window": { "from": "09:00", "to": "18:00" }
}
```

Ответ `201`:

```json
{
  "id": "11111111-1111-1111-1111-111111111111",
  "name": "Смена А",
  "employee_ids": [7, 9],
  "display_window": { "from": "09:00", "to": "18:00" },
  "timezone": "Europe/Moscow",
  "enabled": true,
  "revision": 1,
  "updated_at": "2026-09-11T12:00:00Z",
  "share_url_issued": true,
  "view_key": "abcdefghijklmnopqrstuvwxyz0123456789ABCDE",
  "share_url": "https://activity.example.test/#/p/abcdefghijklmnopqrstuvwxyz0123456789ABCDE"
}
```

Пример PATCH отключения:

```json
{ "revision": 1, "enabled": false }
```

Пример конфликта revision:

```json
{
  "error": {
    "code": "conflict",
    "message": "Панель изменена. Обновите данные и повторите.",
    "request_id": "55555555-5555-4555-8555-555555555555",
    "retryable": false
  }
}
```

### Ошибки Core (envelope Activity)

Тело:

```json
{
  "error": {
    "code": "invalid_argument",
    "message": "Краткое безопасное сообщение",
    "request_id": "55555555-5555-4555-8555-555555555555",
    "retryable": false
  }
}
```

`message` не копирует SQL, upstream и секреты. `retryable` истинно только для `unavailable`, `resource_exhausted`, `deadline_exceeded`.

| HTTP | `error.code` | Когда | UI TeamOS |
| --- | --- | --- | --- |
| 400 | `invalid_argument` | валидация, неизвестное поле, пустой состав, имя вне 1–120, окно не `HH:MM`, >100 сотрудников | показать текст у формы; не молчать |
| 401 | `unauthenticated` | нет/неверный `ACTIVITY_MANAGEMENT_TOKEN` | «не подключён» / ошибка конфигурации сервера. **Не** выкидывать пользователя из сессии TeamOS |
| 403 | `permission_denied` | lifecycle, capability, pilot | нет доступа к новому контролю |
| 403 | `reauth_required` | установке нужна повторная авторизация amoCRM | явная причина, не пустая панель |
| 404 | `not_found` | панель чужого scope или отсутствует | панель недоступна; обновить список |
| 409 | `conflict` | `revision` устарел **или** тот же Idempotency-Key с другим payload | черновик сохранить; предложить reload. Для idempotency — не менять ключ |
| 429 | `resource_exhausted` / limiter | бюджет; заголовок `Retry-After` | подождать и повторить; не крутить бесконечно |
| 503 | `unavailable` | Core policy/DB, Activity DB, Gateway Users, CRM Events | ошибка недоступности, не пустой успех и не выдуманные сотрудники |
| 504 | `deadline_exceeded` | истек deadline | как недоступность; для create/rotate — повтор с тем же Idempotency-Key |

`401` сессии **TeamOS** обрабатывается существующим refresh/login. Его нельзя смешивать с `401` management token.

Недоступность зависимостей — fail-closed (ADR-0024): не показывать пустую панель как успех.

## BFF TeamOS (что добавить в будущем)

Браузерный клиент `src/api/activityV2.ts` ходит **только** на backend TeamOS существующим `httpRequest` / Bearer TeamOS. Префикс proxy не обязан совпадать с Core; зафиксировать его в OpenAPI TeamOS при реализации. Рекомендуемое зеркало, чтобы не изобретать операции:

| Браузер → TeamOS | TeamOS → Core |
| --- | --- |
| `GET /api/v1/activity-v2/scope` | внутренний резолв company → installations; Core не вызывается, если scope пуст |
| `GET /api/v1/activity-v2/panels` | `GET /api/v1/activity/panels` |
| `POST /api/v1/activity-v2/panels` | `POST /api/v1/activity/panels` + свой Idempotency-Key, если клиент его не прислал на BFF |
| `GET /api/v1/activity-v2/panels/{panelId}` | `GET /api/v1/activity/panels/{panelId}` |
| `PATCH /api/v1/activity-v2/panels/{panelId}` | `PATCH /api/v1/activity/panels/{panelId}` |
| `POST /api/v1/activity-v2/panels/{panelId}/share-link` | `POST /api/v1/activity/panels/{panelId}/share-link` |
| `GET /api/v1/activity-v2/employees` | `GET /api/v1/activity/employees` |

BFF подставляет installation/integration из сессии (или из проверенного выбора). Клиент не шлёт management token и не шлёт произвольный `X-Activity-Installation-Id` как доказательство.

Идемпотентность: браузер генерирует UUID на intent создания/перевыпуска и передаёт его BFF; BFF пересылает как `Idempotency-Key`. После потери ответа повторяется тот же UUID.

Query keys (эскиз, не расширять legacy `activity`):

```ts
activityV2: {
  all: ['activity-v2'] as const,
  scope: (companyId: string) => ['activity-v2', companyId, 'scope'] as const,
  panels: (companyId: string, installationId: string) =>
    ['activity-v2', companyId, installationId, 'panels'] as const,
  panel: (companyId: string, installationId: string, panelId: string) =>
    ['activity-v2', companyId, installationId, 'panel', panelId] as const,
  employees: (companyId: string, installationId: string) =>
    ['activity-v2', companyId, installationId, 'employees'] as const,
}
```

Ответ другой компании или другой installation не должен перезаписывать текущий экран. Смена компании во время запроса — отмена/игнор устаревшего результата. `view_key` / `share_url` не класть в query cache списка; держать в состоянии ответа мутации до закрытия диалога.

Отдельного нормализатора «legacy + v2 с fallback» не делать. Схемы не объединять.

## Viewer API (не вызывать из вкладки)

Вкладка TeamOS эти маршруты не использует. Они нужны, чтобы не спутать права и не встроить просмотр в админку.

`Authorization: Bearer <view_key>`. Без `X-Activity-Installation-Id` / `X-Activity-Integration-Id`.

| Метод | Путь |
| --- | --- |
| GET | `/api/v1/activity/view/panel` |
| GET | `/api/v1/activity/view/timeline` |
| GET | `/api/v1/activity/view/employees/{employeeId}` |
| GET | `/api/v1/activity/view/events/{eventId}` |

Query timeline/employee: `from`, `to` (unix seconds, включительно, ≤31 сутки), `limit`, `cursor`, `buckets` = `hour` \| `day` \| `auto`. Категории — как ADR-0015, опционально. Сотрудник вне панели, чужой cursor, чужой event ID → `404`.

Viewer panel DTO (синтетика):

```json
{
  "name": "Смена А",
  "timezone": "Europe/Moscow",
  "display_window": { "from": "09:00", "to": "18:00" },
  "employees": [
    { "id": 7, "name": "Иванов", "group_id": 1, "group_name": "Отдел" }
  ],
  "interpretation_version": 1
}
```

Нет `id` панели, installation/integration, revision, hash, `view_key`, settings retention, management token.

«Открыть» из TeamOS — `window.open(share_url)` без `window.opener` (например `noopener,noreferrer`) на origin прототипа. Не встраивать iframe Core и не проксировать viewer через сессию TeamOS.

## Порядок будущей реализации

Отдельный последующий scope. Этот документ его не закрывает.

1. Проверить живой Core: OpenAPI/контракт APP-02, тестовая компания, `API_BASE_URL`, что CLI `panel-list` / `panel-create` отвечает на той же паре installation/integration. Секрет management token не попадает в фронт и в отчёт.
2. На backend TeamOS: резолв company → installation(s), хранение `ACTIVITY_MANAGEMENT_TOKEN`, BFF-прокси, проверка роли. Без этого UI не подключать.
3. Добавить route `/activity-control-v2`, пункт «Контроль активности — новый», `src/pages/activity-control-v2/`, `src/api/activityV2.ts`, `queryKeys.activityV2`, prefetch и тесты permissions. Legacy вкладку сохранить.
4. Список, создание, редактор сотрудников, revision, идемпотентность, enabled, диалог ссылки после create/rotate.
5. Открытие/копирование и перевыпуск; проверить прототип по новой ссылке при закрытом TeamOS.
6. Проверки ниже и отчёт. Подключение production и перенос старых панелей — не следствие готовности UI.

## Критерии приёмки будущей вкладки

Чеклист исполнителя TeamOS. Пункты не отмечены: APP-04 их не выполняет.

- [ ] Создать две панели с разным составом; каждая ссылка открывает нужную панель и только её сотрудников.
- [ ] Изменить одну панель; другая и настройки старого `/activity-control` не меняются.
- [ ] Повторить создание после потери ответа тем же Idempotency-Key; второй экземпляр не появляется.
- [ ] Проверить конфликт двух редакторов (`409` по `revision`) без молчаливой перезаписи; черновик сохраняется, предлагается reload.
- [ ] Проверить отключение (`enabled=false` → viewer `404`) и перевыпуск (прежний ключ `404`, новый открывается). Viewer key не открывает management API.
- [ ] Проверить другую компанию, чужие panel/employee ID и пользователя без прав (`employee` / `partner`); отказ обеспечивает backend, не только скрытое меню.
- [ ] Несколько installations одной компании: UI требует явный выбор; первая найденная сама не берётся. `amoAccountId` без подтверждённого installation не открывает управление.
- [ ] Браузер не содержит `ACTIVITY_MANAGEMENT_TOKEN` и не шлёт TeamOS Bearer на Activity origin.
- [ ] Проверить loading/empty/error/429+Retry-After, смену компании во время запроса и отсутствие fallback на `src/api/rakurs/activity.ts`.
- [ ] В редакторе нет pipelines, Telegram, «эффективности», двухминутных интервалов и сохранения `{panels,tasks}`.
- [ ] Прототип открывается по `share_url` при закрытом TeamOS и виджете; сбор CRM Events продолжает работать.
- [ ] Проверки нового клиента и регрессия прежней вкладки проходят; в тестах и отчёте только синтетические секреты.

Завершение APP-04 означает готовность задания и принятого API для другого агента. Это **не** выполнение checklist и **не** реализация вкладки.
