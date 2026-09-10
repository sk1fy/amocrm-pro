# ADR-0019: исходящий бюджет Gateway amoCRM

Статус: принято 2026-09-10. Условный межрепличный механизм **не внедряется**.

Дополняет [ADR-0001](0001-postgresql-without-redis.md),
[ADR-0010](0010-activity-v0-service-ownership.md),
[ADR-0011](0011-activity-authorization-budget.md) и
[ADR-0014](0014-event-enrichment-contract.md).

Это решение о бюджете исходящих HTTP к amoCRM. Его нельзя смешивать с
бюджетом живой роли актёра ([ADR-0011](0011-activity-authorization-budget.md)),
ingress limiter виджета/webhook и fair claim очереди Core.

## Решение

Пилот сохраняет **один процесс Core worker/Gateway** как единственного
владельца исходящего API v4 бюджета ([ADR-0010](0010-activity-v0-service-ownership.md)).
`amocrm.Client` в этом процессе держит process-local карты
`golang.org/x/time/rate.Limiter`: 7 rps / burst 7 на `integration_id` и
50 rps / burst 50 на `account_id`. Каждый продуктовый вызов v4 проходит
`waitBudget` до HTTP. Карты **не** являются кластерным бюджетом: второй
экземпляр `Client` или вторая реплика Gateway удвоили бы квоту.

Новый общий лимитер (Redis, внешний sidecar, таблица токенов) **не
вводится**, пока не появится вторая реплика, исполняющая API v4.
Измерения этапа 6 не показывают вытеснения сборщика или пользовательских
действий подробными чтениями. Ставки 7/50 не меняются без нового
измерения.

## Что входит в бюджет

Один объект `amocrm.Client` создаётся в `cmd/worker` и передаётся Gateway,
policy, lead-status и webhook reconcile. Activity и CRM Events amoCRM-клиентов
не строят.

| Путь | Метод клиента | Бюджет |
| --- | --- | --- |
| Collector Events | `ListEvents` → `DoJSON` | да |
| Directory / Users | `GetDirectory` → `DoJSON` (account + до 4 страниц users) | да |
| Живая роль актёра | `GetUserAuthorization` → `DoJSON` | да |
| Lead-status GET | `GetLeadState` → `DoJSON` | да |
| Lead-status PATCH | `PrepareLeadStatus` / `SetLeadStatus` → `waitBudget` + `request` | да |
| Enrichment Notes/Tasks/Pipelines/CustomFields/Entities | `List*` → `DoJSON` | да |
| OAuth bootstrap `GET /api/v4/account` | `BootstrapAccount` → `waitBudget` + `request`; неизвестный аккаунт затем `chargeAccount` | да |
| Webhook list/register/delete | `ListWebhooks` / `RegisterWebhook` / `DeleteWebhook` → `DoJSON` | да |

`CustomFields` может списать до 20 токенов за claim (страницы). Повтор
`401` в `DoJSON` списывает бюджет на каждую попытку. Первое обнаружение
аккаунта использует общий bucket `account_id=0`, затем дебет канонического
ID без возврата при отмене.

## Что не входит

`OAuthClient.ExchangeCode` и `OAuthClient.Refresh` ходят на
`POST /oauth2/access_token`. Это не API v4 и не проходит `waitBudget`.
Параллельный refresh одной установки сериализуется коротким claim/lease
на `oauth_credentials` ([ADR-0017](0017-oauth-refresh-lease.md)): SQL-транзакция
не удерживается на время HTTP. Это исключает storm refresh одной установки.
Отдельный limiter на token endpoint не вводится: Exchange — один вызов на
callback, Refresh — не чаще чем истечение токена плюс forced `401`, без
обнаруженного неограниченного шторма. Token endpoint не смешивается с v4.

`OAuthClient.GetAccount` остаётся методом совместимости без `waitBudget`.
Штатная композиция API подключает `AccountReader` → worker
`BootstrapAccount`, и этот `GET /api/v4/account` **внутри** бюджета.
PHP и сторонний трафик по-прежнему снаружи Go-бюджета; 429 от amoCRM
остаётся возможным.

## Переполнение, 429 и недоступность

Limiter не имеет приоритетной очереди. `Wait` блокирует до токена или
отмены контекста. Отмена до HTTP даёт `amocrm_budget_wait_seconds{canceled}`
и не открывает сокет. После допуска HTTP классифицируется:

| Исход | Поведение | Метрика |
| --- | --- | --- |
| 2xx | успех | `amocrm_requests_total{2xx}` |
| 429 | `ErrorRateLimited`, retryable, `Retry-After` или 5s | `{429}` |
| 401 | refresh наблюдаемой версии либо `reauth_required` | `{401}` |
| 4xx | постоянная / validation / not_found / forbidden | `{4xx}` |
| 5xx / 408 / 409 | `ErrorTemporary`, retryable | `{5xx}` |
| транспорт | ошибка без статуса | `{transport_error}` |

Gateway отображает 429 в `resource_exhausted` с `Retry-After`, дедлайн — в
`deadline_exceeded`, прочее — в `unavailable` (`MapUpstreamError`). Core
jobs (lead-status, webhook reconcile) ставят retry с `Retry-After`. CRM
Events: collector job — `retry` с backoff, учитывающим `Retry-After`;
enrichment — `retry/temporary` без перевода source в `failed`. Приоритет
сборщика над enrichment остаётся на слое claim CRM Events (`EnrichOnce`
только если `Claim` не нашёл работу), не в limiter.

Пользовательские действия не строят отдельную очередь: Issue делает одну
живую `GetUserAuthorization` (ADR-0011), directory кешируется 30 с,
pipelines/fields — 5 мин. Панель `Query`/`GetEvent` amoCRM не вызывает.

## Измерения этапа 6

Повторный прогон не выполнялся. Использованы
[REL-02](../verification/stage6-2026-09-08/rel-02.md) и
[fixes/README.md](../verification/stage6-2026-09-08/fixes/README.md).

- Read-path: Events=Notes=Tasks=Pipelines=CustomFields=Entities=0 на
  Query/GetEvent/Claim. Panel бьёт только Users (справочник).
- Workers under shared budget (5 установок, mock 20 rps burst 1, 20 ms):
  ожидание бюджета P95 76.4 ms / P99 78.4 ms; collector lag ≤ 4.26 s;
  детали P95 2.89 s; 50 Events и 38 Tasks, refresh 13% Tasks-запросов;
  все current/backfill/enrichment завершены. Голодания сборщика нет.
- REL-02: Redis, новые индексы чтения и агрегаты не требуются.

Поэтому приоритетные очереди исходящих запросов не вводятся.

## Будущий механизм при нескольких репликах Gateway

Когда появится **вторая** реплика, исполняющая API v4, выбранный механизм —
**таблица token bucket в PostgreSQL Core**, не Redis и не advisory lock.

Ключи: `(scope_kind, scope_id)` для integration и account. Короткая
транзакция: пополнить по `rate`/`burst` и списать один токен, либо
вернуть wait/exhausted. Реплика вызывает Reserve до HTTP вместо
локального `rate.Limiter`. Это согласуется с ADR-0001 (PostgreSQL без
Redis) и с тем, что Core уже координирует leases и fair claim. Advisory
lock сериализовал бы вызовы до 1, что хуже 7 rps. Внешний sidecar
добавил бы dependency без измерений.

До этого триггера process-local карты остаются достаточными. Fair claim
и object leases уже ограничивают **работу** между репликами worker; они
не заменяют исходящий HTTP-бюджет и не переписываются этой записью.

## Последствия

- Один Gateway-процесс продолжает владеть 7/50.
- Enrichment, directory, Events, bootstrap, lead-status и webhooks делят
  этот бюджет.
- Token endpoint остаётся снаружи; CORE-01 не смешивается с v4.
- Горизонтальное масштабирование Gateway требует отдельной реализации
  выбранной таблицы и новых измерений, не checkbox в текущем пилоте.
