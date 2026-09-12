# ADR-0025: внутренний read-only admin HTTP listener

- **Status:** Accepted for admin panel stage 1.
- **Date:** 2026-09-12.

Дополняет [ADR-0002](0002-two-go-binaries-one-module.md). Не меняет публичный
OAuth/widget/webhook контракт и не заменяет management listener.

## Контекст

Админ-панель живёт в отдельном репозитории и не должна подключаться к
PostgreSQL Core напрямую. Оператору нужен обзор аккаунтов, установок,
интеграций, jobs и аудита. Секреты (OAuth ciphertext, webhook keys,
`payload`/`result` jobs) не должны покидать Core.

Публичный listener (`HTTP_ADDRESS`) и management listener
(`MANAGEMENT_HTTP_ADDRESS`) уже заняты. Management открывает `/live`,
`/ready`, `/metrics` и каталог компонентов; это не admin API.

## Решение

`cmd/api` поднимает **третий** HTTP listener, только если заданы оба
`ADMIN_HTTP_ADDRESS` и `ADMIN_API_TOKEN`. Пустые значения отключают
listener: существующие тесты и процессы без этих переменных продолжают
стартовать с двумя слушателями. Одна переменная без другой — ошибка
`LoadAPI`.

Маршруты `/admin/v1/*` регистрируются только на этом listener пакетом
`internal/adminread`. Публичный и management роутеры их не получают.
Аутентификация: `Authorization: Bearer` и обязательный `X-Admin-Actor`.
Этап 1 — только чтение. Команды появятся позже в `internal/admincommand`.

SQL выбирает явные безопасные колонки. Пагинация — keyset, не `OFFSET`.
Ответы содержат `source: "core"` и `observed_at`.

В Compose listener привязан к `:8083` внутри контейнера; публикация
хоста — только `127.0.0.1`.

## Отклонённые варианты

### Читать PostgreSQL Core из Admin API

Отклонён: нарушает границу владельца данных и выносит схему Core наружу.

### Добавить admin маршруты на публичный или management listener

Отклонён: публичный контракт должен остаться точным; management — health и
метрики, не карточки установок.

### Включать listener по умолчанию на `:8083`

Отклонён: пустой адрес означает «выключено». Иначе process/integration
тесты API потребовали бы `ADMIN_*`.

## Последствия

- Admin API ходит только в Core HTTP, не в PostgreSQL Core;
- третий listener увеличивает поверхность процесса API и должен оставаться
  внутренним (loopback / внутренняя сеть, Bearer token);
- схема Core на этапе 1 не меняется; списки пишутся заново в
  `internal/adminread`, а не копируют SQL мутаций;
- контракт: `api/admin-openapi.yaml`, отдельно от публичного OpenAPI.
