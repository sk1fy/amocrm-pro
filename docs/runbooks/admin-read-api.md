# Admin read API

Внутренний read-only listener Core для админ-панели. Это не публичный API.

## Включение

Listener стартует только когда заданы **оба** значения:

- `ADMIN_HTTP_ADDRESS` — TCP `host:port`, внутри контейнера `:8083`;
- `ADMIN_API_TOKEN` — разделяемый секрет, без значения по умолчанию в
  процессе. Compose для dev ставит
  `ADMIN_API_TOKEN:-admin-dev-token-change-me`.

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

Контракт: [`api/admin-openapi.yaml`](../../api/admin-openapi.yaml).
Решение: [ADR-0025](../adr/0025-admin-read-listener.md).
