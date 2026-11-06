# Admin read API

Внутренний listener Core для админ-панели. Это не публичный API.
Команды и durable-квитанции описаны в [admin-commands.md](admin-commands.md).

## Включение

Listener стартует только когда заданы **оба** значения:

- `ADMIN_HTTP_ADDRESS` — TCP `host:port`, внутри контейнера `:8083`;
- `ADMIN_API_TOKEN` — разделяемый секрет, без значения по умолчанию в
  процессе. Compose для dev ставит
  `ADMIN_API_TOKEN:-admin-dev-token-change-me`.

Публичное значение Compose допускается только при `APP_ENV=development`.
В production и staging задайте собственный `ADMIN_API_TOKEN`: с dev-токеном
процесс откажется стартовать. Значение должно совпадать с токеном Core
адаптера Admin API.

Если обе переменные пустые, listener выключен. Если задана только одна —
процесс API не стартует.

Порт на хосте публикуется только на loopback:
`127.0.0.1:8083` (activity-стек `127.0.0.1:18083`). Healthcheck остаётся
на management `:8082`.

## Вызов

Нужны заголовки:

- `Authorization: Bearer <ADMIN_API_TOKEN>`
- `X-Admin-Actor` — непустой `prefix:value`, например
  `employee:<uuid>` или `automation:<name>`

```sh
curl -sS \
  -H "Authorization: Bearer ${ADMIN_API_TOKEN}" \
  -H "X-Admin-Actor: employee:11111111-1111-1111-1111-111111111111" \
  http://127.0.0.1:8083/admin/v1/backend
```

Без токена ответ `401` и `WWW-Authenticate: Bearer`.

## Что никогда не возвращается

- OAuth access/refresh tokens и любой ciphertext;
- webhook keys и зашифрованные destination URL;
- `payload` и `result` jobs;
- `ADMIN_API_TOKEN` в логах и телах ответов.

## Гарантии списков

- `GET /admin/v1/accounts` возвращает `total` (`count(DISTINCT account_id)`
  по фильтрам); каждая установка несёт `webhook_status`,
  `authorization_state` (вычисленное состояние без секретов) и
  `recent_failed_jobs` (failed/dead за 24 ч), а также `grants` в формате
  `{service, enabled}`; пустой массив означает отсутствие грантов;
- `recent_failed_jobs` с тем же правилом доступен в кратких установках
  `/admin/v1/installations` и в карточках аккаунта/установки;
- `GET /admin/v1/jobs` и `GET /admin/v1/installations/{id}/jobs` сортируют
  по `updated_at` (новые первыми), в каждом job есть `installation_id` и
  `account_id`; окно `since` ограничено 7 сутками;
- Доставки `/admin/v1/installations/{id}/activity/deliveries` возвращают
  последние команды (новые первыми); CLI `ListDeliveries` по-прежнему
  выбирает failed/expired от старых к новым.

## Activity, lead-status и статистика

Listener выдаёт `serviceapi.Auth` с `Kind=operator` и `actor_id=0`
(ADR-0028). Токен делегации не покидает процесс Core: ответы содержат
только факты (`source=core`, `observed_at`). `view_key` и `share_url`
в GET панелей нет.

- `GET .../activity/settings` — `updated_at` 0, если строка настроек
  ещё не сохранялась;
- `GET .../activity/status` — unix-поля CRM Events как RFC3339 UTC
  либо `null`; `state=not_enabled` это 200, отсутствие адаптера —
  `503 backend_unavailable`;
- `GET /admin/v1/stats?period=24h|7d|30d` (по умолчанию 7d) считает
  все метрики в одном запросе. Ноль — реальное значение; неизвестное
  (latency без попыток) — `null`.

## Пользователи amoCRM (listener worker)

`GET /admin/v1/accounts/{account_id}/crm-users` обслуживает не `api`,
а worker: только он ходит в amoCRM ([ADR-0042](../adr/0042-admin-crm-users-read.md)).

- Включение: `ADMIN_CRM_HTTP_ADDRESS` (Compose `:8085`) и тот же
  `ADMIN_API_TOKEN`, что у admin listener. Адрес без токена —
  worker не стартует; без адреса listener выключен.
- Сеть `amocrm-admin-link`, alias `core-admin-crm`; на хосте только
  loopback `127.0.0.1:8085` (activity-стек `127.0.0.1:18085`).
- Заголовки те же: `Authorization: Bearer`, `X-Admin-Actor`.

```sh
curl -sS \
  -H "Authorization: Bearer ${ADMIN_API_TOKEN}" \
  -H "X-Admin-Actor: employee:11111111-1111-1111-1111-111111111111" \
  http://127.0.0.1:8085/admin/v1/accounts/31415926/crm-users
```

Ответ `200` всегда содержит `state`. `ok` — список (`id`, `name`,
`is_active`, `is_admin`) и `fresh_until`; снимок живёт в памяти
worker 5 минут. Остальные состояния — `no_authorized_connection`,
`auth_error`, `rate_limited` (+ `retry_after_seconds`), `incomplete`,
`network_error`, `internal_error` — несут `items: null`: список
неизвестен. Аккаунт без установок — `404`. Email, телефон, группы и
права не возвращаются. Причина отказа пишется в лог worker
(`admin crm users unavailable`, `reason_code`, `upstream_status`).

Контракт: [`api/admin-openapi.yaml`](../../api/admin-openapi.yaml).
Решение: [ADR-0025](../adr/0025-admin-read-listener.md).
Дополнение диагностики: [ADR-0026](../adr/0026-admin-read-diagnostics.md).
Принципал Activity: [ADR-0028](../adr/0028-admin-activity-principal.md).
Пользователи amoCRM: [ADR-0042](../adr/0042-admin-crm-users-read.md).
