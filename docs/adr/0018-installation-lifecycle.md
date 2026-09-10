# ADR-0018: lifecycle установки, отзыва и uninstall

Статус: принято для CORE-02. Дата: 2026-09-10.

Дополняет [ADR-0008](0008-multi-widget-capability-boundary.md) и
[ADR-0011](0011-activity-authorization-budget.md). Enable/disable интеграции,
pilot Activity, pause consumer и live policy уже существуют и не заменяются.

## Решение

Доступ и удаление данных разделены. Operator CLI меняет status интеграции или
установки. Live policy, OAuth credential load и job admit читают эти статусы
на каждом новом запросе. Продуктовые модули не подписываются на шину событий
и не копируют права. Durable product-команда не вводится: текущих guards
достаточно, чтобы запретить новый доступ.

Webhook-подписки — Core-orchestration. Reconcile после OAuth регистрирует
нужный destination и удаляет подтверждённые прежние destinations этой установки. Uninstall
после фиксации `uninstalled` вызывает существующий `DeleteWebhook`. Повтор
uninstall идемпотентен и повторяет remote unregister.

Удаление истории CRM Events / Activity / квитанций не входит в uninstall.
Retention истории событий уже есть у владельца; техническая история — CORE-05.
Отдельной команды purge в этой поставке нет.

## Переходы

| Переход | Доступ к истории | Новые jobs / Issue | In-flight | Receipts / CRM history | Webhooks | Credentials | История amoCRM |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `disable` интеграции | Чтение Activity/CRM Events отклоняется live policy (`n.status='active'`). История в owner DB сохраняется | OAuth start/callback и capability admit этой интеграции запрещены | Уже авторизованная мутация может завершиться до commit disable (ADR-0008). Worker повторяет guard перед side effect | Сохраняются | Remote подписки не снимаются | Client secret и OAuth rows не трогаются | Не меняется |
| `enable` интеграции | Восстанавливается, если installation/pilot/grant тоже active | Новые jobs допускаются; terminal jobs сами не перезапускаются | Нет | Сохраняются | Intent прежний; регистрация — при следующем reconcile | Не меняются | Не меняется |
| `disable-installation` | Live policy: installation не `active` → PermissionDenied | Job admit и token load этой установки запрещены. Другие установки интеграции не затрагиваются | Как у disable интеграции | Сохраняются | Ingress игнорирует (нужен `status=active`); remote не удаляются — операция обратима | Не удаляются; load требует `active`/`authorizing` | Не меняется |
| `enable-installation` | Только из `disabled` → `active`. `uninstalled` этим путём не оживляется | После enable — как у активной установки | Нет | Сохраняются | Ранее оставленные remote подписки снова принимаются, если ключ тот же | Не меняются | Не меняется |
| Pause Activity consumer (`POST /sync kind=disable`) | Чтение истории при живом pilot/capability разрешено | Новые порции сбора не стартуют | Допущенная страница может закончиться | Receipts и owner history сохраняются | Не затрагиваются | Не затрагиваются | Не меняется |
| `activity-control pilot-disable` | Чтения Activity и новые команды запрещены live policy | Новые Issue/порции запрещены | Допущенная страница: до ~10s RPC + запись | Сохраняются | Не затрагиваются | Не затрагиваются | Не меняется |
| `revoke` | Live policy: `reauth_required` | Token load запрещён (не `active`/`authorizing`). Новые jobs не допускаются | Как у disable | Сохраняются | Не снимаются: после OAuth reconcile восстановит нужный destination | Строки `oauth_credentials` остаются; это локальная инвалидация, не remote revoke | Не меняется |
| `uninstall` | Live policy PermissionDenied. История в owner DB не стирается | Status `uninstalled` закрывает token load и job admit существующими guards | Как у disable. Оставшийся `webhook.reconcile` завершается постоянной `installation_not_active` | Jobs, deliveries, credentials, receipts не удаляются | List+Delete только подтверждённых destinations установки; `webhook_status=unregistered` | Не удаляются, но не загружаются | События в amoCRM не трогаются |
| OAuth reauthorization | SaveInstallation (без изменений этой задачи) поднимает ту же пару integration+account в `active` и ставит `webhook.reconcile` | После успешного callback — как у новой установки | Нет | Прежние rows той же installation сохраняются | Reconcile регистрирует desired и удаляет stale duplicates | Новые ciphertext, `token_version+1` | Не меняется |

OAuth start по `integration_code` остаётся integration-scoped. Uninstall одной
установки не выключает виджет целиком: повторная авторизация того же аккаунта —
это reinstall, а не обход disable интеграции.

## Почему нет durable product-команды

`corepolicy` уже требует `i.status='active' AND n.status='active'` и отдельно
распознаёт `reauth_required`. Capability/job admit требует те же статусы.
Pause consumer и pilot — существующие продуктовые/Core рычаги. Для запрета
доступа достаточно этих проверок. Новая очередь «уведомить Activity/CRM Events
об uninstall» копировала бы права и требовала бы worker wiring вне CORE-02.

Единственная Core-очистка с внешним вызовом — webhook unregister. Это
синхронный шаг operator CLI с повтором той же идемпотентной командой. Новый
тип job в worker не регистрируется.

## Удаление данных

Uninstall не является purge. Поздний явный процесс должен:

1. Требовать отдельную команду и явное подтверждение (не флаг uninstall).
2. Соблюдать retention CRM Events (2–30 суток) и горизонт CORE-05 для квитанций.
3. Идти у владельца данных, а не каскадом из Core `installations`.

Пока такой команды нет.

## Владение webhook destinations после аудита 10.09

API возвращает список всего аккаунта. Реестр `installation_webhook_destinations`
(миграция 000015) записывается до внешней регистрации и сохраняет intent при
потере ответа. URL зашифрованы, hash и AAD привязаны к установке. При смене ключа
или hostname прежние подтверждённые URL удаляются; записи успешно удалённых
destinations очищаются. Чужие URL, включая соседнюю установку на том же hostname,
сохраняются. Для старых регистраций без реестра принимается только точный
канонический путь с текущим секретным webhook key этой установки. Старые URL
с утраченными неизвестными ключами автоматически не удаляются.

## Последствия

- CLI: `disable-installation`, `enable-installation`, `uninstall`, `revoke`.
- Remote amoCRM OAuth revoke API не вызывается: клиента нет, это не uninstall.
- Uninstall использует общий OAuth refresh по capability, привязанной к UUID
  установки со статусом uninstalled. Истёкший access обновляется, если refresh
  ещё валиден; продуктовый доступ остаётся закрытым. Неизвестный исход refresh
  требует восстановления pending либо reauthorization, а не повторной отправки
  того же одноразового token (ADR-0017).
- Конкурентный OAuth callback после commit uninstall может снова сделать
  установку `active` — это reinstall через существующий upsert.
