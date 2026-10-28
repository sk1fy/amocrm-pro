# Регистрация MVP «Контроля активности 2.0»

Объём КА-01.01: продукт `activity`, интеграция и видимость в админке.
Решение: [ADR-0040](../adr/0040-activity-mvp-registration.md).

## Идентичность

Через внутренний Admin API сопоставьте `integration.code`,
`integration.client_id`, `installation.integration_id` и аккаунт.
`client_id` — UUID, выданный amoCRM; `integration.id` — UUID Core.
Код пакета `rkrs_activity_panel` не заменяет идентичность JWT.
Не создавайте дубль, если UUID уже зарегистрирован под другим кодом.

Существующие маршруты:

- `/admin/v1/integrations` и `/admin/v1/integrations/{id}`;
- `/admin/v1/accounts/{accountId}`;
- `/admin/v1/installations/{id}`.

Нужны внутренний admin listener и серверный bearer. Не копируйте
bearer, OAuth или JWT в отчёт, браузер либо командную строку.
Админка читает данные через серверный адаптер.

## Подключение

Если интеграции нет, следуйте [общей инструкции](integrations.md):
audited `create`, настоящий client UUID, callback URI,
`--services activity`. Client secret передаётся через stdin.
Development credentials не подходят для реальной установки.

Если интеграция существует, проверьте её grant `activity`.
Для включения используйте `set-service` конкретного согласованного
кода. Env bootstrap не изменяет существующий grant.
OAuth проходит по этой же интеграции. Capability и pilot
включаются отдельно: [инструкция Activity](activity-v0.md).
Отключение pilot не отзывает OAuth и не отключает интеграцию.

## Переустановка из amoМаркета

Отключение виджета отзывает токены: Core переводит установку
в `reauth_required`, виджет получает 401 (`tenant_not_found`).
При повторной установке amoCRM сам вызывает callback с `code`,
`referer`, `client_id` и `from_widget`, но без `state`. Core
принимает такой callback только для активной интеграции с этим
`client_id`, один раз на код (повтор — 400) и с доменом amoCRM в
`referer`. Остальные callback по-прежнему требуют `state`.
Установка и её `installation_id` сохраняются, статус снова
`active`; `crm-events` возобновляет сбор примерно в течение минуты.
Ручной `/oauth/amocrm/start` и `activity-sync` не нужны.

Проверка: в журнале API `GET /oauth/amocrm/callback` → 201,
затем запросы виджета 200; `event_sources` уходит из
`reauth_required` без команды.

## Приёмка

1. Откройте аккаунт → виджеты. Проверьте продукт, подключение,
   grant, pilot и OAuth. Недоступность источника показана явно.
2. Перейдите к установке и интеграции, сопоставьте client UUID
   с настоящей интеграцией amoCRM.
3. На согласованной тестовой установке свежий SDK JWT активного
   сотрудника открывает `GET /api/v1/widget/activity/state`
   и `GET /api/v1/widget/activity/sessions`. На каждый запрос
   нужен свежий одноразовый JWT. Администратор для собственной
   истории не требуется; неактивный сотрудник получает отказ.
4. Сотрудник не получает административный browser-session.
   Доступ администратора требует актуальной роли.
5. Выключенный pilot, отозванный grant и несовпадение интеграций
   проверяйте изолированно. Не отключайте живую установку ради теста.

Тест Core связывает audited provisioning, сохранение OAuth
с синтетическими credentials, JWT и политику сотрудника.
Он не подтверждает живой обмен OAuth или установленный SDK.
Результаты живых проверок фиксируются отдельно.
Развёртывание/ZIP — КА-01.04; ручная приёмка — КА-01.06.
