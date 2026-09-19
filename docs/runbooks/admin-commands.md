# Команды админки: запуск, квитанции и восстановление

Контракт: `api/admin-openapi.yaml`. Решение:
[ADR-0027](../adr/0027-admin-command-receipts.md).

## Подготовка

1. Применить Core миграции, включая `000016_admin_commands`.
2. Обновить worker с handlers `admin.connection_check` и
   `admin.webhook_unregister`, затем API. Старый worker не должен получать
   новые admin jobs.
3. Включить внутренний listener по [admin-read-api.md](admin-read-api.md),
   используя собственный `ADMIN_API_TOKEN` вне development.
4. Передать API `PUBLIC_BASE_URL` с HTTPS origin Core. Он используется для
   ссылки повторной OAuth-авторизации; особенно важен при redirect через relay.

Пилот и production не обновляются автоматически командами проверки.

## Выполнение

Admin API проверяет роль сотрудника и отправляет Bearer,
`X-Admin-Actor: employee:<uuid>` и `Idempotency-Key`. Рекомендуемый ключ — UUID
уже сохранённой операции Admin API; он же ID Core-квитанции.

Пример тела команды:

```json
{
  "target_type": "installation",
  "target_id": "11111111-1111-4111-8111-111111111111",
  "command": "check",
  "payload": {}
}
```

Маршрут — `POST /admin/v1/commands`. `200` содержит terminal-квитанцию,
`202` — pending/running с `job_id`. `GET /admin/v1/commands/{id}` возвращает
результат после перезагрузки Admin API и не вызывает amoCRM.

| Объект | Команды | Payload |
| --- | --- | --- |
| installation | enable, disable, revoke, uninstall, reconcile, check, pilot-enable, pilot-disable | `{}` |
| installation | activity-configure | initial_days, retention_days; expected_updated_at необязателен (0 = ещё не сохраняли) |
| installation | activity-sync | kind=enable\|sync\|backfill\|disable; from/to только для backfill |
| installation | activity-panel-create | name, employee_ids, display_window; enabled необязателен |
| installation | activity-panel-patch | panel_id, revision; name/employee_ids/display_window/enabled по желанию |
| installation | activity-panel-rotate | panel_id; новый секрет остаётся в Activity |
| installation | lead-status-configure | source/target pipeline и status, enabled, expected_revision |
| integration | create | code, client_id, client_secret, redirect_uri, services; webhook_events необязателен; target_id=`new` |
| integration | update | redirect_uri и/или webhook_events |
| integration | rotate-secret | client_secret |
| integration | enable, disable | `{}` |
| integration | set-service | service, enabled |
| job | retry | `{}` |
| delivery | retry | installation_id, проверяемый против владельца команды |

Секрет задаётся только при create/rotate-secret. Он не возвращается,
не записывается в квитанцию/audit/job payload и не должен попадать в shell
history или логи прокси. Изменение выполняет владелец данных Core через
существующие прикладные функции.

## Интерпретация результата

- `succeeded` — команда выполнена или безопасный job поставлен в очередь.
  Для reconcile/retry это подтверждение постановки; результат самого job
  проверяется отдельно по его ID.
- У проверки `succeeded` означает, что сохранена классификация:
  `verified_ok`, `auth_error`, `network_error`, `rate_limited`,
  `internal_error`. `result.observed_at` — время проверки,
  `result.retry_after` — секунды ожидания при 429.
- `failed` с `error.code=conflict` — Core отклонил предусловие. Например,
  enable восстанавливает только disabled, а revoke не разрешён для
  disabled/uninstalled.
- `partial` uninstall — локальный uninstalled уже зафиксирован, но снятие
  webhook не завершено. Прочитать `webhook_error` и проверить объект.
- `unknown_outcome` — worker не сохранил подтверждённый результат. Не
  запускать команду автоматически снова; проверить объект/квитанцию.

Revoke — локальная инвалидация, а не отзыв токена в amoCRM. Ответ содержит
`oauth_start_url` для повторной авторизации. Ротация секрета не включает
отключённую интеграцию; отключение интеграции не меняет статусы установок.

## Потерянный ответ и повтор

После обрыва Admin API читает заранее известный ID квитанции. Не хранить
секрет для повторной отправки. Тот же ключ/запрос возвращает первую
квитанцию независимо от сотрудника; другой payload с этим ключом — 409.
Другая команда над объектом с незавершённой квитанцией получает 409.
Перед приёмом новой команды Core обновляет pending/running `activity-sync`
объекта через bridge: подтверждённый успех или отказ освобождает объект
без предварительного GET. Работающий sync и недоступный источник статуса
сохраняют блокировку. Обновление идёт до транзакции, затем занятость снова
проверяется под advisory lock
([ADR-0031](../adr/0031-admin-sync-reconciliation.md)).

Для явного повторного uninstall после partial используется новая операция
и новый ключ. Состояние uninstalled сохраняется, а native unregister
снова сверяет собственные webhook destinations. При неизвестном исходе
OAuth refresh сначала восстановить pending refresh либо авторизацию.

Job retry разрешён только для `webhook.reconcile` и `widget.ping` в
failed/dead при active установке и интеграции. Использовать
`retry_allowed`/`retry_reason` из read API; Core проверяет их заново при
команде. Старые попытки и principal сохраняются, добавляется пять попыток.
Activity retry допускается только для failed доставки моложе семи суток.

Команды Activity (ADR-0028) идут от `Kind=operator`, `actor_id=0`, без
`used_widget_tokens`. Токены делегации в квитанции не попадают.
`activity-configure` применяет CAS `expected_updated_at` сразу в
Activity DB. `activity-sync` остаётся `pending`, пока outbox не
`accepted` и операция CRM Events не завершится. Rotate панели из
админки инвалидирует старые ссылки; URL нужно брать через
`activity-control`.

## Проверки

```sh
make fmt-check vet test openapi-check integration-test activity-ci
```

Integration-тесты используют отдельный PostgreSQL: cross-actor replay,
изменённый payload/секрет, атомарность audit, pending-конкуренция,
worker completion/lease expiry, partial uninstall, owner-bound delivery,
безопасный retry и сохранение исходного principal.
