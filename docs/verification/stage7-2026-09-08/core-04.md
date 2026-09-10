# CORE-04: исходящий бюджет amoCRM

Дата: 10 сентября 2026. HEAD: `dac12af`. Это проверка текущего
single-owner бюджета, **не** внедрение межрепличного limiter и **не**
повтор REL-02.

План: этап 7, CORE-04. Решение: [ADR-0019](../../adr/0019-gateway-outbound-budget.md).
Результат условного межрепличного механизма: **изменение не требуется**.

Не путать с [ADR-0011](../../adr/0011-activity-authorization-budget.md) и
`internal/corepolicy/authorization_budget_integration_test.go` — там бюджет
живой роли актёра (один `GetUserAuthorization` на Issue), не RPS amoCRM.

## Состав

Один `amocrm.Client` создаётся в `cmd/worker/main.go` и передаётся в
`componentruntime.StartGateway`, lead-status и webhook reconcile. Ставки
`newLimiter(7, 7, 50, 50)` не менялись. `waitBudget` списывает integration
и account buckets до HTTP. Метрики: `amocrm_budget_wait_seconds{admitted|canceled}`,
`amocrm_requests_total` с конечным набором outcome.

Activity и CRM Events amoCRM-клиентов не создают. Fair claim
(`internal/jobs/fair_claim.go`) ограничивает leases между worker replicas;
CORE-04 — исходящий HTTP, claim не переписывался.

## Матрица путей × бюджет

| Продуктовый путь | Код | HTTP amoCRM | `waitBudget` / общий `Client` | Вне бюджета |
| --- | --- | --- | --- | --- |
| Collector Events | `Client.ListEvents` ← Gateway `Events` ← CRM Events `Claim` | `GET /api/v4/events` | да, через `DoJSON` | нет |
| Directory / Users | `Client.GetDirectory` ← Gateway `Users` ← Activity panel | `GET /api/v4/account?with=…`, `GET /api/v4/users` до 4 стр. | да, каждый `DoJSON` | нет |
| Живая роль | `Client.GetUserAuthorization` ← Policy `Issue` | `GET /api/v4/users/{id}` | да | нет |
| Lead-status чтение | `GetLeadState` / `GetUserAuthorization` | `GET /api/v4/leads/{id}`, `GET /api/v4/users/{id}` | да | нет |
| Lead-status мутация | `PrepareLeadStatus` / `SetLeadStatus` | `PATCH /api/v4/leads/{id}` | да, прямой `waitBudget` (не `DoJSON`) | нет |
| Enrichment Notes | `ListNotes` ← Gateway `Notes` | `GET /api/v4/{entity}/notes?filter[id]` | да | нет |
| Enrichment Tasks | `ListTasks` ← Gateway `Tasks` | `GET /api/v4/tasks?filter[id]` | да | нет |
| Enrichment Pipelines | `ListPipelines` ← Gateway `Pipelines` | `GET /api/v4/leads/pipelines` | да | нет |
| Enrichment CustomFields | `ListCustomFields` ← Gateway `CustomFields` | `GET /api/v4/{entity}/custom_fields` до 20 стр. | да, страница = один токен | нет |
| Enrichment Entities | `ListEntities` ← Gateway `Entities` | `GET /api/v4/{entity}?filter[id]` | да | нет |
| Bootstrap account | `Client.BootstrapAccount` ← Gateway `CoreBootstrap.GetAccount` ← API `AccountReader` | `GET /api/v4/account` | да; discovery затем `chargeAccount` | нет |
| Webhook reconcile | `ListWebhooks` / `RegisterWebhook` / `DeleteWebhook` | `GET/POST/DELETE /api/v4/webhooks` | да | нет |
| OAuth Exchange | `OAuthClient.ExchangeCode` ← API callback | `POST /oauth2/access_token` | нет | да, token endpoint |
| OAuth Refresh | `OAuthClient.Refresh` ← `TokenProvider` | `POST /oauth2/access_token` | нет | да, token endpoint |
| Legacy `OAuthClient.GetAccount` | не штатный путь при `AccountReader` | `GET /api/v4/account` | нет | да, только если reader не подключён |
| PHP / сторонние интеграции | вне этого процесса | любые | нет | да |

Обход `waitBudget` на продуктовых v4 путях **не найден**. Enrichment
добавлен в этапе 3 через `DoJSON`. Код limiter/ставок не менялся.

Проверки общего бюджета:

- `TestSharedBudgetAcrossExistingWidgetsAndEvents` — Events, user, lead GET, PATCH.
- `TestBootstrapKnownAccountSharesExistingIntegrationAndAccountBudgets`,
  `TestBootstrapDiscoveryDebitsCanonicalAccountBeforeNormalTraffic`.
- `TestEnrichmentAndDirectoryShareExistingClientBudget` — Notes/Tasks/Pipelines/Entities
  и directory делят тот же 7/7 limiter, что и Events.

## Вытесняют ли подробные чтения сбор и действия

Повторный load test не запускался. Источник:
[rel-02.md](../stage6-2026-09-08/rel-02.md),
[fixes/README.md](../stage6-2026-09-08/fixes/README.md),
[workers.json](../stage6-2026-09-08/fixes/rel-02/workers.json).

| Наблюдение | Вывод |
| --- | --- |
| Query/GetEvent/Claim не вызывают Gateway Events/Notes/Tasks/Pipelines/CustomFields/Entities | Карточка и панель не конкурируют с amoCRM на read-path |
| Panel/EventCard: только Users (1–2 справочника на измерение размеров) | Directory в бюджете, но кеш 30 с |
| `TestStage6WorkersUnderSharedBudget`: P95 wait 76.4 ms, max collector lag 4.26 s, детали P95 2.89 s, 50 Events + 38 Tasks, все 5 backfill и все детали ready | Enrichment не оставил collector без работы на этом профиле |
| REL-02: Redis/новые индексы не нужны | Не вводятся и здесь |

Приоритет сборщика уже есть на claim CRM Events, не в limiter. Отдельная
очередь исходящих запросов не вводится.

## Переполнение / 429 / unavailable

1. `rate.Limiter.Wait` ждёт токен. Отмена контекста: HTTP нет, `canceled`.
2. Допуск: HTTP, затем `classifyResponse`.
3. 429 → `ErrorRateLimited` retryable, `Retry-After` (иначе 5s), метрика `429`.
   Gateway: `resource_exhausted`. Core jobs: `jobs.Retryable` с задержкой.
   CRM Events collector: `retry` + backoff/`Retry-After`. Enrichment:
   `retry/temporary`, source не `failed`.
4. 5xx/408/409 → `ErrorTemporary`. Транспорт → `transport_error` /
   `unavailable`. Дедлайн wait или вызова → `deadline_exceeded`.
5. 401: один refresh наблюдаемой версии, затем `reauth_required`.

Нет drop-on-overflow на limiter и нет приоритетов внутри `waitBudget`.

## OAuth token endpoint

`POST /oauth2/access_token` не входит в 7/50. Конкурентный Refresh одной
установки сериализуется `FOR UPDATE` на `oauth_credentials` (транзакция
сейчас держится на время сети — CORE-01). Это уже не шторм. Отдельный
limiter на OAuth-клиент не добавлялся. CORE-01 заменит длинную транзакцию
коротким lease; это координация credentials, не v4-бюджет.

Штатный bootstrap account идёт через worker `BootstrapAccount` и **в**
бюджете. `OAuthClient.GetAccount` — запасной метод без limiter.

## Несколько реплик Gateway

Process-local maps не координируют replicas. Выбранный **будущий**
механизм (только при второй реплике, исполняющей v4): таблица token bucket
в PostgreSQL Core. Не реализован. Redis не вводится (ADR-0001, вывод REL-02).

## Что сделано в этой задаче

- ADR-0019 и строка в `docs/adr/README.md`.
- Этот отчёт.
- Тест, что enrichment/directory делят существующий limiter.
- Код ставок, Redis, fair_claim, OAuth token_provider **не** менялись.

## Проверка

```sh
go test -count=1 -timeout=60s ./internal/integration/amocrm/
```

Без PostgreSQL, docker-compose, `make activity-ci` и повторного REL-02.

## Остаток

- Межрепличный bucket — при появлении второй Gateway-реплики.
- CORE-01 — убрать сеть из SQL-транзакции refresh; не часть CORE-04.
- PHP/сторонний трафик по-прежнему вне Go-бюджета; 429 возможны.
- Живой amoCRM и production SLO здесь не измерялись.
