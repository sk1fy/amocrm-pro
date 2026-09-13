# ADR 0028: Административный принципал Activity

Статус: принято, 2026-09-13.

## Контекст

Порты Activity и CRM Events требуют `serviceapi.Auth` (ADR-0011).
Сейчас Issue выполняется для:

- пользователя amoCRM (виджетный JWT, live `GetUserAuthorization`);
- `Kind=viewer` (share link);
- `Kind=operator` (management token + scope installation/integration)
  только с грантами `activity/panels` и `gateway/users`.

Admin listener (`ADMIN_API_TOKEN` + `X-Admin-Actor`) не выдаёт
делегацию. Сотрудник админки не является пользователем amoCRM, поэтому
user-Issue с live role lookup неприменим. Без принципала
`SyncStatus`/настройки/sync из админки остаются `unknown`.

## Решение

1. Расширить `allowedGrant` для `PrincipalKindOperator`: кроме панелей
   — `activity/settings`, `activity/operation`, `crm-events/status`,
   `crm-events/sync`, `crm-events/operation`. Management HTTP по-прежнему
   запрашивает только `UserGrantsFor(activity, panels)`.
2. Admin listener выдаёт `serviceapi.Auth` через Policy `Issue` с
   `Kind=operator`, `ActorID=0`, `Scope` из строки установки
   (installation + integration), гранты — `UserGrantsFor` для
   конкретного действия. `X-Admin-Actor` — только аудит Core
   (`actor_type=admin`); в JWT Activity он не попадает.
3. Токен делегации не покидает процесс Core. Admin API видит только
   факты (`Settings`, `SyncStatus`, панели, операции).
4. `CheckDelegation` обязателен (pilot, capability, active,
   `reauth_required`). Live amoCRM role lookup не выполняется.
5. Команды settings/sync идут в существующий outbox
   `activity_command_receipts/outbox` с `actor_id=0`, без
   `used_widget_tokens`. Delivery worker при `actor_id=0` делает Issue
   с `Kind=operator`, иначе — как сейчас, с `ActorID`.
6. Конкурентность настроек: в `Settings` добавляется `updated_at`
   (unix, 0 = строка не создана). `SettingsCommand.expected_updated_at`
   опционален; если задан и не совпал — `conflict` и текущие значения.
   Виджетный путь поле не передаёт и сохраняет last-write-wins по
   `command_id`.
7. Lead-status: admin вызывает прикладной CAS `RuleStore` с
   `actor_type=admin` и `actor_id=X-Admin-Actor`. Live amoCRM admin
   не требуется: право сотрудника проверяет Admin API
   (`leadstatus:rules:write`). Гарантии ADR-0007 (revision CAS,
   нет hard delete) сохраняются.

## Отклонённые варианты

- Новый `Kind=admin`: дублирует operator (нет amoCRM user, только
  Core DB revocation).
- Вызов публичных `/api/v1/activity/*` с management token из Admin
  API: лишний hop, секрет management на стороне админки, нет
  settings/sync грантов.
- User-Issue от имени фиктивного ActorID: ломает ADR-0011 (live
  amoCRM admin) и путает аудит.

## Последствия

- Публичный widget/OAuth API не меняется.
- Operator Kind становится пригодным для диагностики коллекции, не
  только панелей; management token сам по себе settings/sync не
  получает.
- Admin без включённого Activity-пилота получает
  `permission_denied` на портах — это честный отказ, не `unknown`.
