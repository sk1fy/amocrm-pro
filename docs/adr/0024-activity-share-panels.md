# ADR-0024: панели Activity, ссылка просмотра и principal зрителя

- **Status:** Accepted for stage 9 (APP-01). Widget admin-only access is unchanged.
- **Date:** 2026-09-11.

Дополняет [ADR-0010](0010-activity-v0-service-ownership.md),
[ADR-0011](0011-activity-authorization-budget.md),
[ADR-0012](0012-separable-product-module-contract.md) и
[ADR-0015](0015-activity-presentation-contract.md).
Не заменяет widget JWT и не восстанавливает отменённый эксперимент
`siteauth` / `/api/v1/site/*` / `activity_site_sessions`.

Полный mapping полей и HTTP/DTO: [activity-panel-v1.md](../specs/activity-panel-v1.md).

## Контекст

Основной клиент этапа 9 — приложение, которое открывает настроенную панель по
ссылке. Управление панелями — отдельное право будущей вкладки TeamOS.
Зрителю не нужен вход в amoCRM на каждый запрос. Нельзя выдавать зрителя за
администратора, отключать Core policy или запускать новый lookup роли amoCRM
на каждую карточку.

TeamOS company ID, amoCRM account ID и Core installation UUID — разные
идентичности. `amo_account_id` из JSON не является доказательством прав.

## Решение

### Владельцы

| Объект | Владелец |
| --- | --- |
| Панель, состав сотрудников, окно отображения, revision, hash ключа ссылки | Activity |
| История событий, сбор, покрытие | CRM Events (без изменений) |
| Lifecycle installation/capability/pilot, подпись delegation, публичный HTTP | Core |
| Справочник сотрудников | Gateway Users, как у widget panel |

Один installation — одна общая история для всех его панелей. Удаление или
отключение панели не останавливает сбор. Несколько панелей с пересекающимся
составом не дублируют события в CRM Events.

### HTTP-адаптер

Публичный вход зрителя размещается в **Core API**, рядом с widget routes, но
с отдельным middleware. Перенос процесса Activity не меняет origin приложения
и не требует второго публичного TLS-входа.

- Widget URL `/api/v1/widget/activity/*` не меняются.
- Viewer URL: `/api/v1/activity/view/*`. Ключ — `Authorization: Bearer`, не
  query string и не тело GET. Неизвестный, отозванный и отключённый ключ дают
  одинаковый `404`.
- Management URL: `/api/v1/activity/panels*` и `/api/v1/activity/employees`.
  Браузер приложения и ключ просмотра эти маршруты не вызывают.
- CORS зрителя — точные HTTPS origin из `ACTIVITY_APP_ORIGINS`. Пустой список
  отключает viewer HTTP (fail-closed). Widget CORS остаётся списком установок.
- Management CORS не нужен: вызывают TeamOS backend и CLI, не браузер.

### Principal и grants

`IssueRequest.Kind` аддитивен. Пустой Kind сохраняет прежнее поведение.

| Kind | ActorID | System | amoCRM role lookup | Grants |
| --- | --- | --- | --- | --- |
| `""` (widget user) | >0 | false | да, admin-only | как ADR-0011 |
| `"viewer"` | 0 | false | нет | `activity/view`, `crm-events/read`, `gateway/users` |
| `"operator"` | 0 | false | нет | `activity/panels`, `gateway/users` |
| system collector | 0 | true | нет | без изменений; Core по-прежнему не выпускает system |

Viewer Issue выполняет Core после `ResolveShare` у Activity. В claims —
`panel_id` и `view_key_version`. Validate на каждом hop по-прежнему вызывает `CheckDelegation`
(installation, capability, pilot, `reauth_required`). Отзыв ключа
дополнительно проверяет Activity по hash и `view_key_version` на каждый
просмотр; версия берётся из подписанной delegation и сверяется с owner DB.
Delegation без версии отвергается: задержка отзыва ключа не привязана к 30s snapshot роли.

Операторский Kind выпускает Core для CLI и будущего backend TeamOS.
Локальный секрет `ACTIVITY_MANAGEMENT_TOKEN` — stand-in доверенного
межсервисного вызова, не пользовательская сессия и не widget JWT. Браузеру
этот секрет не выдаётся. Заголовки scope:
`X-Activity-Installation-Id`, `X-Activity-Integration-Id`.

Ключ просмотра не даёт settings/sync/backfill, список всех панелей,
чужих сотрудников, management API или секреты.

### Ключ ссылки

- `panel_id` — UUID, выдаёт сервер. Это не секрет.
- Ключ — 32 криптостойких байта, `base64url` без padding. Timestamp-ID
  старого клиента секретом не является.
- В БД хранится только SHA-256. Сырой ключ возвращается один раз при
  создании и перевыпуске. Логи, metrics labels, fixture и отчёты ключ не
  содержат.
- Перевыпуск увеличивает `view_key_version`, меняет hash, прежняя ссылка
  недействительна сразу на следующем чтении Activity.
- Отключение панели (`enabled=false`) сохраняет ряд и hash, но просмотр
  отвечает `404`. Сбор CRM Events не зависит от `enabled`.

### Управление

Синхронный CRUD (HTTP 200/201), без фиктивной очереди. Идемпотентность
`Idempotency-Key` для create и rotate; PATCH требует `revision` и даёт
`409` при конфликте. Одна панель меняется отдельно, весь список не
перезаписывается.

Лимиты: ≤50 панелей на installation, 1–100 сотрудников, имя 1–120
символов, период чтения как у widget query (≤31 суток). Часовой пояс —
timezone аккаунта из Gateway directory на момент создания; зритель его не
подменяет. Фильтр воронок, двухминутные интервалы, Telegram, Imbox,
«эффективность» и сравнение сделок в этот контракт не входят.

### Недоступность

| Зависимость | Поведение |
| --- | --- |
| Core policy / DB | fail-closed: `unavailable` или `permission_denied`, не пустая панель |
| CRM Events | `unavailable`; не ноль событий |
| Gateway Users | `unavailable` для состава имён; не выдумывать сотрудников |
| Activity DB | `unavailable` |

Живой amoCRM role lookup для зрителя и оператора не выполняется.

### Бюджет viewer HTTP

До ResolveShare действует общий бюджет одного Core API: 100 запросов/с,
burst 200. Он общий для разных значений Bearer, включая неизвестные ключи.
Дополнительно сохраняется 20 запросов/с, burst 40 на ключ. Кеш ограничен
4096 записями, неактивные ключи очищаются после 5 минут (проверка раз в минуту).
При заполнении кеша новые ключи получают 429, активные записи не вытесняются
с обнулением их бюджета. Эти пределы относятся к одному процессу пилота.

## Последствия

- Нужны owner-миграция Activity, новые actions/grants, protobuf RPCs,
  OpenAPI, CLI seed и отдельный CORS/rate-limit для viewer.
- Widget admin-only и существующие URL сохраняются.
- Реализация вкладки TeamOS этим ADR не выполняется.
