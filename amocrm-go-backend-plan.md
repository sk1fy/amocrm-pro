# План backend-платформы для виджетов и интеграций amoCRM на Go

> Версия документа: 1.0  
> Архитектура: два Go-сервиса — `api` и `worker`  
> Основное хранилище: PostgreSQL  
> Дополнительное хранилище: Redis — опционально  
> Внешний вход: Traefik, Nginx или облачный Load Balancer

## 1. Цель проекта

Создать backend-платформу, через которую можно:

- подключать интеграцию к нескольким аккаунтам amoCRM;
- устанавливать и обслуживать JS-виджеты;
- принимать запросы из интерфейса amoCRM;
- выполнять действия через API amoCRM;
- принимать Webhooks amoCRM;
- запускать фоновые сценарии и синхронизации;
- безопасно хранить OAuth-токены;
- масштабировать HTTP API и фоновые задачи независимо.

План предполагает **multi-tenant** модель: одна backend-платформа обслуживает много аккаунтов amoCRM, а каждая установка интеграции является отдельным tenant-контекстом.

Основной сценарий документа — публичная интеграция с JS-виджетом. Для внешней интеграции, где `client_id` и `client_secret` выдаются отдельно для каждой установки, модель `Integration` и хранилище credentials нужно расширить установочными OAuth-ключами.

---

## 2. Главные архитектурные решения

### 2.1. Два сервиса вместо большого набора микросервисов

На первом этапе достаточно двух независимо запускаемых Go-приложений:

1. `amocrm-api` — публичный HTTP API.
2. `amocrm-worker` — фоновая обработка и обращения к amoCRM API.

Это два отдельных бинарника и два отдельных deployment, но их удобно держать в одном репозитории и одном Go-модуле.

### 2.2. Балансировщик не является бизнес-сервисом

Traefik, Nginx или облачный Load Balancer должен:

- завершать TLS;
- маршрутизировать домены и URL;
- ограничивать размер запросов;
- ставить базовые rate limits;
- распределять запросы между экземплярами `amocrm-api`.

Писать собственный балансировщик на Go не нужно.

### 2.3. Отдельный `auth-service` на старте не нужен

Внутри платформы есть несколько разных механизмов авторизации:

- OAuth 2.0 amoCRM для доступа backend к API аккаунта;
- одноразовый JWT amoCRM для запросов из JS-виджета;
- авторизация собственной панели управления, если она будет создана;
- service-to-service доступ между внутренними компонентами.

На первом этапе это должны быть отдельные модули внутри `amocrm-api`, а не самостоятельный микросервис.

### 2.4. PostgreSQL как источник истины и первая очередь задач

Для MVP PostgreSQL используется одновременно как:

- основное хранилище;
- inbox входящих Webhooks;
- очередь фоновых задач;
- хранилище состояния workflow;
- журнал попыток и ошибок.

RabbitMQ, NATS или Kafka можно подключить позже, если объём событий станет слишком большим или появится несколько независимых потребителей.

---

## 3. Общая архитектура

```mermaid
flowchart LR
    Widget[JS-виджет amoCRM]
    Amo[amoCRM API и Webhooks]
    Dashboard[Панель управления]
    Edge[Traefik / Nginx / Load Balancer]
    API[amocrm-api]
    DB[(PostgreSQL)]
    Redis[(Redis, опционально)]
    Worker[amocrm-worker]
    External[Внешние сервисы]

    Widget -->|HTTPS + одноразовый JWT| Edge
    Dashboard -->|HTTPS + session/JWT| Edge
    Amo -->|OAuth callback / Webhooks| Edge
    Edge --> API

    API --> DB
    API -.-> Redis

    Worker --> DB
    Worker -.-> Redis
    Worker -->|Bearer access token| Amo
    Worker --> External
```

### 3.1. Ответственность компонентов

| Компонент | Ответственность |
|---|---|
| `amocrm-api` | OAuth callback, API виджета, приём Webhooks, настройки, пользовательские сессии, постановка задач |
| `amocrm-worker` | вызовы amoCRM API, обновление токенов, обработка Webhooks, workflow, retry, синхронизация |
| PostgreSQL | установки, токены, inbox, jobs, настройки, аудит |
| Redis | распределённый rate limit, replay-защита JWT, короткий кэш, блокировки |
| Traefik/Nginx | TLS, routing, внешние лимиты, балансировка |

---

## 4. Границы сервисов

### 4.1. `amocrm-api`

Публичный сервис, доступный из интернета.

#### Обязанности

- принимать OAuth redirect;
- проверять `state` и завершать установку;
- принимать запросы JS-виджета;
- проверять одноразовые JWT amoCRM;
- принимать стандартные Webhooks amoCRM;
- сохранять входящие события до отправки успешного ответа;
- создавать фоновые jobs;
- отдавать состояние jobs;
- хранить и изменять настройки установки;
- обслуживать собственную панель управления;
- отдавать `/live`, `/ready` и `/metrics`.

#### Чего сервис не делает

- не выполняет долгие workflow;
- не синхронизирует большие объёмы данных;
- не делает несколько последовательных запросов в amoCRM в рамках Webhook-запроса;
- не отправляет тяжёлые запросы во внешние сервисы;
- не ждёт завершения фоновой операции перед ответом виджету.

---

### 4.2. `amocrm-worker`

Внутренний сервис без публичного бизнес-API.

#### Обязанности

- забирать jobs из PostgreSQL;
- выполнять запросы к amoCRM API;
- обновлять access/refresh token;
- регистрировать, обновлять и удалять Webhooks;
- нормализовать и обрабатывать Webhook-события;
- выполнять workflow;
- повторять временно неудачные операции;
- выполнять периодическую синхронизацию;
- контролировать API rate limits;
- переводить безнадёжные jobs в `dead`;
- отдавать внутренние `/live`, `/ready` и `/metrics`.

#### Сетевой доступ

Worker не публикуется в интернет. Доступ к его health/metrics endpoint разрешается только из внутренней сети.

---

## 5. Основные доменные сущности

### `Integration`

Описание продукта или интеграции:

- `client_id`;
- зашифрованный `client_secret`;
- `redirect_uri`;
- код интеграции;
- поддерживаемые Webhook-события;
- параметры виджета.

Для одной публичной интеграции запись обычно одна.

### `Installation`

Установка интеграции в конкретный аккаунт amoCRM:

- внутренний UUID;
- `account_id` amoCRM;
- домен аккаунта;
- статус установки;
- пользователь, установивший интеграцию;
- настройки tenant;
- ссылка на OAuth credentials;
- состояние Webhook-подписки.

### `OAuthCredentials`

Секретные данные установки:

- access token;
- refresh token;
- время истечения access token;
- версия токена;
- время последнего refresh;
- версия ключа шифрования.

### `WebhookDelivery`

Один HTTP-запрос, полученный от amoCRM:

- installation;
- raw body;
- Content-Type;
- SHA-256 body;
- время приёма;
- результат разбора;
- correlation ID.

### `InboxEvent`

Одно нормализованное событие из Webhook-запроса:

- тип сущности;
- тип события;
- ID сущности;
- event timestamp;
- normalized payload;
- deduplication key;
- статус обработки.

### `Job`

Фоновая операция:

- тип;
- payload;
- tenant;
- статус;
- число попыток;
- приоритет;
- время следующего запуска;
- lease/lock;
- result или error.

---

## 6. OAuth 2.0 и установка интеграции

### 6.1. Состояния установки

```text
pending
  -> authorizing
  -> active
  -> reauth_required
  -> disabled
  -> uninstalled
  -> error
```

### 6.2. OAuth-поток

```mermaid
sequenceDiagram
    participant U as Пользователь
    participant A as amoCRM
    participant API as amocrm-api
    participant DB as PostgreSQL
    participant W as amocrm-worker

    U->>A: Установить / предоставить доступ
    A->>API: GET /oauth/amocrm/callback?code=...&state=...
    API->>DB: Проверить и одноразово погасить state
    API->>A: POST /oauth2/access_token
    A-->>API: access_token + refresh_token + expires_in
    API->>DB: Зашифровать и сохранить токены
    API->>DB: Создать installation и job webhook.reconcile
    API-->>U: Установка принята
    W->>DB: Забрать webhook.reconcile
    W->>A: POST /api/v4/webhooks
    A-->>W: Webhook зарегистрирован
    W->>DB: installation.status = active
```

Authorization code amoCRM действует ограниченное время, поэтому callback нельзя откладывать в обычную низкоприоритетную очередь. Код следует обменять сразу в API либо создать высокоприоритетную job с жёстким SLA. Срок access token нельзя хардкодить: backend сохраняет `expires_in`, полученный от amoCRM.

### 6.3. Проверка `state`

Перед редиректом на OAuth создаётся случайный `state`:

```text
state = 32 случайных байта
TTL = 10–20 минут
single-use = true
```

В БД или Redis хранится:

```json
{
  "state_hash": "sha256(...) ",
  "installation_draft_id": "uuid",
  "return_url": "/settings/integration",
  "expires_at": "...",
  "consumed_at": null
}
```

При callback необходимо:

1. найти `state` по хэшу;
2. проверить срок действия;
3. проверить, что он ещё не использован;
4. пометить использованным в одной транзакции;
5. только после этого обменивать authorization code.

### 6.4. Хранение токенов

Токены нельзя хранить открытым текстом.

Рекомендуемый вариант:

```text
plaintext token
    -> AES-256-GCM data key
    -> ciphertext + nonce
    -> data key защищён master key/KMS
```

В логах запрещено выводить:

- `Authorization`;
- access token;
- refresh token;
- client secret;
- полный OAuth callback URL;
- расшифрованные credentials.

### 6.5. Обновление refresh token

Refresh token amoCRM одноразовый: после успешного обмена старый refresh token больше нельзя использовать. В документации также указано, что при отсутствии обновления в течение трёх месяцев доступ будет утрачен и потребуется повторная авторизация. Поэтому обновление должно выполняться под блокировкой на конкретную установку, а платформа должна отслеживать давно неактивные installations.

Алгоритм:

```text
1. Прочитать credentials.
2. Если access token ещё достаточно свежий — вернуть его.
3. Получить lock по installation_id.
4. Повторно прочитать credentials после lock.
5. Проверить, не обновил ли токен другой Worker.
6. Выполнить refresh.
7. В одной транзакции сохранить новый access token и новый refresh token.
8. Увеличить token_version.
9. Освободить lock.
```

Варианты блокировки:

```sql
SELECT *
FROM oauth_credentials
WHERE installation_id = $1
FOR UPDATE;
```

или PostgreSQL advisory lock.

Правило повторной обработки `401`:

```text
API request -> 401
    -> один принудительный refresh
    -> один повтор исходного запроса
    -> если снова 401, installation = reauth_required
```

Бесконечный retry на `401` запрещён.

---

## 7. Запросы из JS-виджета

Для запросов из Web-интерфейса amoCRM следует использовать одноразовый JWT, который добавляется методом авторизованного запроса Web SDK.

Backend должен проверять:

- алгоритм подписи;
- подпись секретом интеграции;
- `iss`;
- `aud`;
- `client_uuid`;
- `account_id`;
- `user_id`;
- `iat`, `nbf`, `exp`;
- уникальность `jti`.

### Replay-защита

`jti` сохраняется на время жизни токена:

```text
widget-jti:{client_uuid}:{jti} -> 1
TTL = exp - now
```

Повторный JWT с тем же `jti` отклоняется.

### Контекст запроса

После успешной проверки middleware создаёт:

```go
type WidgetPrincipal struct {
    AccountID  int64
    UserID     int64
    ClientUUID string
    Issuer     string
    TokenID    string
}
```

Далее любой handler обязан получить `installation` по сочетанию:

```text
client_uuid + account_id
```

Нельзя доверять `account_id`, переданному отдельно в JSON body.

### Пример API

```http
GET  /api/v1/widget/bootstrap
POST /api/v1/widget/actions/send-lead
POST /api/v1/widget/actions/add-note
GET  /api/v1/jobs/{job_id}
```

Асинхронная операция возвращает:

```http
HTTP/1.1 202 Accepted
Content-Type: application/json
```

```json
{
  "job_id": "be942d04-dfad-4633-863f-8481ad859d58",
  "status": "queued"
}
```

---

## 8. Webhooks amoCRM

### 8.1. Разделение двух операций

#### Входящая доставка

amoCRM вызывает твой публичный endpoint:

```text
amoCRM -> Traefik/Nginx -> amocrm-api
```

#### Управление подпиской

Worker вызывает amoCRM API:

```text
amocrm-worker -> POST/GET/DELETE /api/v4/webhooks -> amoCRM
```

### 8.2. Публичный endpoint

Рекомендуемый маршрут:

```http
POST /hooks/amocrm/v1/{webhook_key}
```

Полный destination:

```text
https://hooks.example.com/hooks/amocrm/v1/{webhook_key}
```

Для каждой установки создаётся отдельный `webhook_key`.

Требования к ключу:

- минимум 32 криптографически случайных байта;
- генерировать через `crypto/rand`;
- передавать в URL как base64url или hex;
- хранить хэш для входящего поиска;
- исходное значение хранить зашифрованным только если оно нужно для reconcile/rotation;
- скрывать путь из access-логов.

### 8.3. Регистрация Webhook

Worker выполняет:

```http
POST https://{account_domain}/api/v4/webhooks
Authorization: Bearer {access_token}
Content-Type: application/json
```

```json
{
  "destination": "https://hooks.example.com/hooks/amocrm/v1/SECRET",
  "settings": [
    "add_lead",
    "update_lead",
    "status_lead",
    "delete_lead",
    "add_contact",
    "update_contact"
  ]
}
```

Лучше регистрировать **один destination на установку** и передавать в `settings` все нужные события, а не создавать отдельный URL для каждого события. amoCRM разрешает подписать один Webhook на несколько событий и документирует ограничение до 100 Webhooks на аккаунт.

Доступность управления Webhooks через API зависит от возможностей тарифа аккаунта; это нужно проверять и отражать понятной ошибкой в интерфейсе установки.

Регистрацией должен заниматься пользователь с административными правами аккаунта. Если OAuth-токен принадлежит пользователю без нужных прав, installation переводится в состояние `error` или `reauth_required`, а пользователю показывается понятная инструкция.

### 8.4. Reconcile подписок

Периодическая задача `webhook.reconcile`:

```text
1. Получить ожидаемый destination и settings из БД.
2. Выполнить GET /api/v4/webhooks?filter[destination]=...
3. Если Webhook отсутствует — создать.
4. Если settings отличаются — выполнить POST с ожидаемыми settings.
5. Если disabled=true — зафиксировать проблему и попытаться восстановить.
6. Обновить checked_at и actual settings.
```

Периодичность для MVP:

```text
каждые 6–24 часа
```

Также reconcile запускается:

- после OAuth установки;
- после изменения настроек;
- после ротации webhook key;
- после ошибки доставки;
- вручную из административной панели.

### 8.5. Формат входящего запроса

Стандартные CRM Webhooks приходят как:

```http
Content-Type: application/x-www-form-urlencoded
```

Пример:

```text
leads[update][0][id]=123456
&leads[update][0][status_id]=142
&leads[update][0][last_modified]=1710000000
&account[id]=987654
```

Не нужно пытаться декодировать такой запрос как JSON.

### 8.6. Критическое требование по времени ответа

amoCRM ждёт ответ Webhook endpoint не более двух секунд. Поэтому handler должен только:

```text
1. Проверить secret route.
2. Ограничить размер body.
3. Прочитать raw body.
4. Минимально проверить формат.
5. Надёжно сохранить delivery и job в PostgreSQL.
6. Вернуть 204.
```

Целевой внутренний бюджет:

```text
p95 < 300 ms
p99 < 800 ms
hard application timeout ≈ 1.5 s
```

Нельзя возвращать `2xx`, пока событие не сохранено в надёжное хранилище. Если PostgreSQL временно недоступен, лучше вернуть `503`, чтобы amoCRM повторила доставку.

Официальная схема повторов после неуспешной доставки:

| Попытка | Пауза после предыдущей | Условие |
|---:|---:|---|
| 1 | сразу | событие произошло |
| 2 | 5 минут | код `0–99` либо `300+` |
| 3 | 15 минут | код `0–99` либо `300+` |
| 4 | 15 минут | предыдущий ответ `499` или `5xx` |
| 5 | 1 час | предыдущий ответ `499` или `5xx` |

Если за последние два часа накоплено более 100 невалидных откликов и последний отклик также невалиден, amoCRM может отключить Webhook. Поэтому latency и долю `5xx` нужно контролировать отдельными алертами.

### 8.7. Коды ответа

| Ситуация | Код |
|---|---:|
| Событие сохранено | `204` |
| Неизвестный `webhook_key` | `404` |
| Метод не поддерживается | `405` |
| Body слишком большой | `413` |
| Временная ошибка PostgreSQL | `503` |
| Валидный endpoint, но событие не поддерживается | сохранить и вернуть `204` |

Для синтаксически повреждённого payload разумно сохранить raw body со статусом `invalid` и вернуть `204`, чтобы не создавать бесконечный retry-шум. Это решение нужно зафиксировать как продуктовую политику.

### 8.8. Безопасность Webhook endpoint

В документации стандартных CRM Webhooks не описана HMAC-подпись запроса. Поэтому входящий запрос следует считать неподписанным и защищать несколькими слоями:

1. секретный непредсказуемый URL;
2. TLS;
3. поиск installation по SHA-256 ключа;
4. проверка `account[id]` против installation;
5. ограничение размера body;
6. edge rate limit;
7. отсутствие секрета в access-логах;
8. дедупликация;
9. аудит аномальной активности.

`account[id]` является только дополнительной проверкой, а не секретом.

### 8.9. Дедупликация

Webhook-доставка имеет семантику at-least-once: при неуспешном ответе amoCRM повторяет запрос. Поэтому одно событие может прийти несколько раз.

Один HTTP payload может содержать несколько событий. Handler сохраняет delivery, а Worker разбивает его на отдельные `inbox_events`.

Пример deduplication key:

```text
sha256(
    installation_id +
    entity_type +
    event_type +
    entity_id +
    event_timestamp +
    normalized_payload_hash
)
```

Ограничение:

```sql
UNIQUE (installation_id, deduplication_key)
```

Если у события нет надёжного timestamp, используется комбинация raw body hash и небольшого временного окна. Это best-effort защита, поэтому сами бизнес-операции также должны быть идемпотентными.

### 8.10. Обработка события Worker-ом

```text
webhook delivery
    -> parse
    -> normalize
    -> deduplicate
    -> определить workflow
    -> при необходимости дочитать сущность через amoCRM API
    -> проверить актуальность updated_at
    -> выполнить действие
    -> сохранить результат
```

Webhook payload не следует считать полной и гарантированно актуальной моделью сущности. Для критичных решений Worker получает актуальную сделку, контакт или задачу через API.

### 8.11. Защита от циклов

Типичный цикл:

```text
Webhook update_lead
    -> Worker изменяет lead
    -> amoCRM отправляет update_lead
    -> Worker снова изменяет lead
```

Защита:

- перед update сравнивать желаемое и фактическое состояние;
- не отправлять update, если данные уже совпадают;
- создавать idempotency key workflow;
- сохранять ожидаемый результат своей операции;
- ограничивать число повторных запусков одного сценария;
- при необходимости использовать служебное поле/метку интеграции.

---

## 9. Go-handler для Webhook

Упрощённый каркас:

```go
package webhook

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "io"
    "mime"
    "net/http"
    "net/url"

    "github.com/go-chi/chi/v5"
)

const maxWebhookBody = 2 << 20 // 2 MiB

type Installation struct {
    ID        string
    AccountID int64
    Enabled   bool
}

type Store interface {
    FindInstallationByWebhookKeyHash(
        ctx context.Context,
        keyHash string,
    ) (Installation, error)

    SaveDeliveryAndEnqueue(
        ctx context.Context,
        installation Installation,
        contentType string,
        rawBody []byte,
        parsed url.Values,
    ) error
}

type Handler struct {
    store Store
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
    key := chi.URLParam(r, "webhookKey")
    if key == "" {
        http.NotFound(w, r)
        return
    }

    sum := sha256.Sum256([]byte(key))
    keyHash := hex.EncodeToString(sum[:])

    installation, err := h.store.FindInstallationByWebhookKeyHash(
        r.Context(),
        keyHash,
    )
    if err != nil || !installation.Enabled {
        http.NotFound(w, r)
        return
    }

    mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
    if err != nil || mediaType != "application/x-www-form-urlencoded" {
        http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
        return
    }

    r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
    rawBody, err := io.ReadAll(r.Body)
    if err != nil {
        var maxErr *http.MaxBytesError
        if errors.As(err, &maxErr) {
            http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
            return
        }
        http.Error(w, "cannot read body", http.StatusBadRequest)
        return
    }

    parsed, parseErr := url.ParseQuery(string(rawBody))
    if parseErr != nil {
        // В production raw body можно сохранить как invalid delivery.
        http.Error(w, "invalid form", http.StatusBadRequest)
        return
    }

    if err := h.store.SaveDeliveryAndEnqueue(
        r.Context(),
        installation,
        mediaType,
        rawBody,
        parsed,
    ); err != nil {
        http.Error(w, "temporary unavailable", http.StatusServiceUnavailable)
        return
    }

    w.WriteHeader(http.StatusNoContent)
}
```

Production-версия дополнительно должна:

- использовать короткий DB timeout;
- добавлять request ID;
- не логировать `webhookKey`;
- проверять `account[id]`;
- сохранять delivery и job в одной транзакции;
- различать временные и постоянные ошибки;
- иметь метрики latency и response codes.

---

## 10. Очередь jobs в PostgreSQL

### 10.1. Статусы

```text
queued
processing
retry
completed
failed
dead
cancelled
```

### 10.2. Таблица

```sql
CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    installation_id UUID REFERENCES installations(id),
    type TEXT NOT NULL,
    status TEXT NOT NULL,
    priority SMALLINT NOT NULL DEFAULT 100,
    payload JSONB NOT NULL,
    result JSONB,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    run_after TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by TEXT,
    locked_until TIMESTAMPTZ,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX jobs_ready_idx
    ON jobs (priority, run_after, created_at)
    WHERE status IN ('queued', 'retry');
```

### 10.3. Получение jobs

```sql
WITH selected AS (
    SELECT id
    FROM jobs
    WHERE status IN ('queued', 'retry')
      AND run_after <= now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY priority ASC, run_after ASC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE jobs AS j
SET status = 'processing',
    locked_by = $2,
    locked_until = now() + interval '60 seconds',
    attempts = attempts + 1,
    updated_at = now()
FROM selected
WHERE j.id = selected.id
RETURNING j.*;
```

`locked_until` нужен, чтобы job вернулся в очередь после падения Worker.

Для долгих jobs Worker обновляет lease heartbeat.

### 10.4. Retry policy

Пример:

```text
attempt 1 -> 5 секунд
attempt 2 -> 30 секунд
attempt 3 -> 2 минуты
attempt 4 -> 10 минут
attempt 5 -> dead
```

С jitter:

```go
func RetryDelay(attempt int, base time.Duration) time.Duration {
    exponent := time.Duration(1 << min(attempt, 8))
    delay := base * exponent
    jitter := time.Duration(rand.Int64N(max(1, int64(delay/4))))
    return delay + jitter
}
```

Повторять:

- network timeout;
- connection reset;
- `429`;
- `500`, `502`, `503`, `504`;
- временную недоступность внешнего сервиса;
- конфликт distributed lock.

Не повторять автоматически:

- ошибки валидации;
- отсутствие прав, пока не изменилось состояние установки;
- неизвестный action;
- повреждённый payload;
- бизнес-ошибки, которые не изменятся от повтора.

### 10.5. Примеры типов jobs

```text
oauth.exchange_code
oauth.refresh_token

webhook.reconcile
webhook.unregister
webhook.parse
webhook.process_event

amocrm.get_lead
amocrm.update_lead
amocrm.add_note
amocrm.create_task

sync.account
sync.leads
sync.contacts

workflow.execute
workflow.compensate
```

---

## 11. Клиент amoCRM API

### 11.1. Один общий клиентский слой

```go
type Client interface {
    GetLead(ctx context.Context, installationID string, leadID int64) (Lead, error)
    UpdateLead(ctx context.Context, installationID string, command UpdateLead) error
    AddNote(ctx context.Context, installationID string, command AddNote) error
    RegisterWebhook(ctx context.Context, installationID string, spec WebhookSpec) error
    DeleteWebhook(ctx context.Context, installationID string, destination string) error
}
```

Бизнес-код не должен самостоятельно собирать HTTP-запросы к amoCRM.

### 11.2. Middleware клиента

Порядок:

```text
request
 -> validate trusted account domain
 -> load access token
 -> per-integration rate limit
 -> per-account rate limit
 -> request ID / tracing
 -> HTTP request
 -> classify response
 -> optional refresh on 401
 -> metrics
```

### 11.3. Ограничение запросов

amoCRM документирует лимиты на частоту API-запросов. При одном Worker можно начать с in-memory token bucket. При нескольких Worker общий limiter нужно вынести в Redis.

Ключи:

```text
ratelimit:integration:{client_uuid}
ratelimit:account:{account_id}
```

### 11.4. Классификация ошибок

```go
type ErrorKind string

const (
    ErrorUnauthorized ErrorKind = "unauthorized"
    ErrorForbidden    ErrorKind = "forbidden"
    ErrorPayment      ErrorKind = "payment_required"
    ErrorRateLimited  ErrorKind = "rate_limited"
    ErrorValidation   ErrorKind = "validation"
    ErrorTemporary    ErrorKind = "temporary"
    ErrorPermanent    ErrorKind = "permanent"
)
```

Пример политики:

| Ответ | Действие |
|---|---|
| `401` | один refresh и один повтор |
| `402` | пометить account limitation, не делать бесконечный retry |
| `403` | проверить права/блокировку, остановить job |
| `404` | обычно permanent, зависит от операции |
| `422` | permanent validation error |
| `429` | retry с backoff и снижением concurrency |
| `5xx` | retry с backoff |

---

## 12. Схема PostgreSQL

### 12.1. `installations`

```sql
CREATE TABLE installations (
    id UUID PRIMARY KEY,
    integration_id UUID NOT NULL,
    account_id BIGINT NOT NULL,
    account_domain TEXT NOT NULL,
    status TEXT NOT NULL,
    installed_by BIGINT,
    settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    webhook_key_hash BYTEA UNIQUE,
    webhook_key_ciphertext BYTEA,
    webhook_settings JSONB NOT NULL DEFAULT '[]'::jsonb,
    webhook_checked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (integration_id, account_id)
);
```

### 12.2. `oauth_credentials`

```sql
CREATE TABLE oauth_credentials (
    installation_id UUID PRIMARY KEY
        REFERENCES installations(id) ON DELETE CASCADE,
    access_token_ciphertext BYTEA NOT NULL,
    refresh_token_ciphertext BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    token_version BIGINT NOT NULL DEFAULT 1,
    key_version INTEGER NOT NULL,
    refreshed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 12.3. `webhook_deliveries`

```sql
CREATE TABLE webhook_deliveries (
    id UUID PRIMARY KEY,
    installation_id UUID NOT NULL
        REFERENCES installations(id),
    request_id UUID NOT NULL,
    content_type TEXT NOT NULL,
    raw_body BYTEA NOT NULL,
    body_sha256 BYTEA NOT NULL,
    parse_status TEXT NOT NULL DEFAULT 'pending',
    parse_error TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    parsed_at TIMESTAMPTZ
);

CREATE INDEX webhook_deliveries_pending_idx
    ON webhook_deliveries (received_at)
    WHERE parse_status = 'pending';
```

### 12.4. `inbox_events`

```sql
CREATE TABLE inbox_events (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL
        REFERENCES webhook_deliveries(id),
    installation_id UUID NOT NULL
        REFERENCES installations(id),
    entity_type TEXT NOT NULL,
    event_type TEXT NOT NULL,
    entity_id BIGINT,
    event_at TIMESTAMPTZ,
    payload JSONB NOT NULL,
    deduplication_key BYTEA NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (installation_id, deduplication_key)
);
```

### 12.5. `job_attempts`

```sql
CREATE TABLE job_attempts (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES jobs(id),
    attempt INTEGER NOT NULL,
    worker_id TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    outcome TEXT,
    error_code TEXT,
    error_message TEXT,
    duration_ms BIGINT
);
```

### 12.6. `audit_log`

```sql
CREATE TABLE audit_log (
    id BIGSERIAL PRIMARY KEY,
    installation_id UUID,
    actor_type TEXT NOT NULL,
    actor_id TEXT,
    action TEXT NOT NULL,
    object_type TEXT,
    object_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

---

## 13. Транзакционная модель Webhook inbox

Сохранение delivery и создание parse job должны происходить в одной транзакции:

```sql
BEGIN;

INSERT INTO webhook_deliveries (...)
VALUES (...)
RETURNING id;

INSERT INTO jobs (
    id,
    installation_id,
    type,
    status,
    priority,
    payload
)
VALUES (
    gen_random_uuid(),
    $installation_id,
    'webhook.parse',
    'queued',
    10,
    jsonb_build_object('delivery_id', $delivery_id)
);

COMMIT;
```

Так не возникает состояния:

```text
delivery сохранён, но job потерян
```

или:

```text
job создан, но delivery не существует
```

---

## 14. Структура Go-репозитория

```text
amocrm-platform/
├── cmd/
│   ├── api/
│   │   └── main.go
│   └── worker/
│       └── main.go
│
├── internal/
│   ├── app/
│   │   ├── api/
│   │   └── worker/
│   │
│   ├── domain/
│   │   ├── installation/
│   │   ├── credentials/
│   │   ├── webhook/
│   │   ├── job/
│   │   └── workflow/
│   │
│   ├── usecase/
│   │   ├── oauth/
│   │   ├── widget/
│   │   ├── webhook/
│   │   ├── sync/
│   │   └── workflow/
│   │
│   ├── integration/
│   │   └── amocrm/
│   │       ├── client.go
│   │       ├── oauth.go
│   │       ├── token_provider.go
│   │       ├── webhook_api.go
│   │       ├── webhook_parser.go
│   │       ├── errors.go
│   │       └── ratelimit.go
│   │
│   ├── transport/
│   │   └── http/
│   │       ├── router.go
│   │       ├── middleware/
│   │       ├── oauth_handler.go
│   │       ├── widget_handler.go
│   │       └── webhook_handler.go
│   │
│   ├── platform/
│   │   ├── postgres/
│   │   ├── redis/
│   │   ├── crypto/
│   │   ├── queue/
│   │   ├── observability/
│   │   └── config/
│   │
│   └── testkit/
│
├── api/
│   └── openapi.yaml
├── migrations/
├── deployments/
│   ├── docker-compose.yaml
│   ├── traefik/
│   └── kubernetes/
├── scripts/
├── Dockerfile.api
├── Dockerfile.worker
├── Makefile
├── go.mod
└── go.sum
```

### Правило зависимостей

```text
transport -> usecase -> domain
integration/platform -> реализуют интерфейсы usecase/domain
```

Domain не должен импортировать:

- HTTP router;
- PostgreSQL driver;
- Redis client;
- amoCRM SDK;
- конкретный logger.

---

## 15. HTTP API

### 15.1. Системные endpoints

```http
GET /live
GET /ready
GET /metrics
```

`/metrics` не должен быть доступен всему интернету.

### 15.2. OAuth

```http
GET /oauth/amocrm/start
GET /oauth/amocrm/callback
POST /api/v1/installations/{id}/reauthorize
```

### 15.3. Widget API

```http
GET  /api/v1/widget/bootstrap
GET  /api/v1/widget/settings
PUT  /api/v1/widget/settings
POST /api/v1/widget/actions/{action}
GET  /api/v1/jobs/{job_id}
```

### 15.4. Webhooks

```http
POST /hooks/amocrm/v1/{webhook_key}
```

### 15.5. Административное API

```http
GET  /api/v1/installations
GET  /api/v1/installations/{id}
POST /api/v1/installations/{id}/webhooks/reconcile
POST /api/v1/installations/{id}/webhooks/rotate
GET  /api/v1/installations/{id}/jobs
GET  /api/v1/installations/{id}/events
```

---

## 16. Redis: когда и для чего

Redis необязателен в первой локальной версии, но нужен при горизонтальном масштабировании.

Использование:

```text
widget JWT jti replay protection
shared rate limits
short-lived OAuth state
distributed locks
short cache of account metadata
worker heartbeat
```

Не хранить только в Redis:

- OAuth credentials;
- единственную копию job;
- единственную копию Webhook;
- критичные настройки;
- audit log.

---

## 17. Безопасность

### 17.1. Разделение DB-ролей

`api_db_role`:

- чтение installations без токенов;
- вставка webhook deliveries;
- вставка jobs;
- чтение безопасных настроек.

`worker_db_role`:

- чтение jobs;
- доступ к зашифрованным credentials;
- изменение installation state;
- запись результатов workflow.

API не обязательно должен иметь право расшифровывать refresh token после завершения первичного OAuth обмена.

### 17.2. SSRF-защита

`account_domain` нельзя принимать из произвольного запроса клиента и сразу использовать как URL.

Нужно:

- получать домен из доверенного OAuth-контекста;
- нормализовать hostname;
- проверять его по allowlist поддерживаемых доменов amoCRM;
- запрещать IP-адреса, localhost и приватные сети;
- запрещать пользовательские схемы и порты;
- всегда использовать HTTPS.

### 17.3. Логи и персональные данные

По умолчанию не логировать полный Webhook body, так как там могут быть телефоны, email и другие персональные данные.

Структурный лог:

```json
{
  "request_id": "...",
  "installation_id": "...",
  "account_id": 123,
  "delivery_id": "...",
  "job_id": "...",
  "event_type": "update_lead",
  "entity_id": 456,
  "status": "accepted"
}
```

Raw body хранится с ограниченным доступом и retention policy.

### 17.4. Секреты

- secrets только через Secret Manager/KMS/environment injection;
- не хранить production secrets в `.env` репозитория;
- поддерживать key version;
- иметь процедуру ротации client secret;
- иметь процедуру ротации webhook key;
- иметь процедуру принудительной reauthorization.

---

## 18. Наблюдаемость

### 18.1. Логи

Обязательные поля:

```text
service
version
environment
request_id
trace_id
installation_id
account_id
job_id
delivery_id
event_id
operation
attempt
error_kind
```

### 18.2. Метрики

```text
http_requests_total
http_request_duration_seconds
webhook_requests_total
webhook_persist_duration_seconds
webhook_invalid_total
webhook_duplicate_total
jobs_ready_total
job_duration_seconds
job_retries_total
job_dead_total
amocrm_requests_total
amocrm_request_duration_seconds
amocrm_401_total
amocrm_429_total
oauth_refresh_total
oauth_refresh_failed_total
```

### 18.3. Алерты

- Webhook `5xx` выше нормы;
- p95 Webhook latency больше 1 секунды;
- oldest queued job старше допустимого SLA;
- рост `dead` jobs;
- массовые ошибки refresh token;
- всплеск `401`, `403`, `429`;
- Webhook subscription отсутствует или disabled;
- PostgreSQL connection pool saturation;
- Worker не обновляет heartbeat.

---

## 19. Тестирование

### 19.1. Unit tests

- parsing `x-www-form-urlencoded` вложенных массивов;
- normalizer событий;
- deduplication key;
- retry classifier;
- token expiry calculation;
- JWT claims validation;
- workflow idempotency.

### 19.2. Integration tests

С реальным PostgreSQL-контейнером:

- сохранение delivery и job в одной транзакции;
- `FOR UPDATE SKIP LOCKED` при нескольких Worker;
- lease recovery после падения Worker;
- блокировка одновременного token refresh;
- unique deduplication constraint.

### 19.3. Fake amoCRM server

Тестовые сценарии:

```text
200 success
401 then successful refresh
401 after refresh
402 subscription restriction
403 permissions
429 rate limit
500/502/503/504
slow response
dropped connection
invalid JSON
```

### 19.4. Webhook acceptance tests

- ответ быстрее двух секунд;
- повтор одинакового payload;
- один payload с несколькими событиями;
- неизвестный webhook key;
- неверный account ID;
- oversized body;
- PostgreSQL unavailable;
- Worker crash после внешнего side effect;
- защита от webhook loop.

---

## 20. Развёртывание

### 20.1. MVP

```text
Traefik/Nginx
1–2 x amocrm-api
1 x amocrm-worker
PostgreSQL
Redis optional
```

### 20.2. Docker Compose для разработки

```yaml
services:
  api:
    build:
      context: .
      dockerfile: Dockerfile.api
    environment:
      DATABASE_URL: postgres://app:app@postgres:5432/app?sslmode=disable
      REDIS_ADDR: redis:6379
    depends_on:
      postgres:
        condition: service_healthy

  worker:
    build:
      context: .
      dockerfile: Dockerfile.worker
    environment:
      DATABASE_URL: postgres://worker:worker@postgres:5432/app?sslmode=disable
      REDIS_ADDR: redis:6379
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres
    environment:
      POSTGRES_DB: app
      POSTGRES_USER: app
      POSTGRES_PASSWORD: app
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d app"]
      interval: 5s
      timeout: 3s
      retries: 20
    volumes:
      - postgres-data:/var/lib/postgresql/data

  redis:
    image: redis

volumes:
  postgres-data:
```

В production версии образов должны быть закреплены, а секреты не должны находиться в compose-файле.

### 20.3. Масштабирование

API:

```text
Load Balancer
  -> api-1
  -> api-2
  -> api-3
```

Worker:

```text
PostgreSQL jobs
  -> worker-1
  -> worker-2
  -> worker-3
```

При нескольких Worker:

- rate limiter должен быть распределённым;
- token refresh должен быть защищён DB lock;
- jobs должны иметь lease;
- обработчики должны быть идемпотентными.

---

## 21. Этапы разработки

### Этап 0. Каркас проекта

- [ ] Создать Go module.
- [ ] Добавить `cmd/api` и `cmd/worker`.
- [ ] Добавить config validation.
- [ ] Добавить structured logging.
- [ ] Добавить graceful shutdown.
- [ ] Поднять PostgreSQL локально.
- [ ] Настроить миграции.
- [ ] Добавить `/live` и `/ready`.

**Результат:** два запускаемых сервиса и воспроизводимое окружение.

### Этап 1. Установки и OAuth

- [ ] Создать `integrations` и `installations`.
- [ ] Реализовать OAuth state.
- [ ] Реализовать OAuth callback.
- [ ] Реализовать обмен authorization code.
- [ ] Добавить шифрование токенов.
- [ ] Реализовать token provider.
- [ ] Реализовать refresh под блокировкой.
- [ ] Добавить installation state machine.

**Результат:** аккаунт amoCRM можно подключить и безопасно обслуживать.

### Этап 2. Клиент amoCRM

- [ ] Общий HTTP client.
- [ ] Timeout и connection pooling.
- [ ] Bearer authorization.
- [ ] Error classification.
- [ ] Retry policy.
- [ ] Per-account/per-integration limiter.
- [ ] Метрики запросов.

**Результат:** Worker стабильно вызывает API amoCRM.

### Этап 3. Webhook subscription

- [ ] Генерация webhook key.
- [ ] `POST /api/v4/webhooks`.
- [ ] `GET /api/v4/webhooks`.
- [ ] `DELETE /api/v4/webhooks`.
- [ ] Job `webhook.reconcile`.
- [ ] Состояние подписки в БД.
- [ ] Ротация webhook key.

**Результат:** платформа сама поддерживает нужные подписки.

### Этап 4. Webhook ingress

- [ ] Публичный endpoint.
- [ ] Body limit.
- [ ] Secret URL lookup.
- [ ] Проверка account ID.
- [ ] Durable inbox.
- [ ] Быстрый `204`.
- [ ] Parser form-urlencoded.
- [ ] Нормализация событий.
- [ ] Дедупликация.

**Результат:** Webhooks надёжно принимаются и не теряются.

### Этап 5. PostgreSQL jobs и Worker

- [ ] Таблица jobs.
- [ ] `SKIP LOCKED` consumer.
- [ ] Lease и heartbeat.
- [ ] Retry + jitter.
- [ ] Job attempts.
- [ ] Dead-letter состояние.
- [ ] Graceful shutdown Worker.

**Результат:** тяжёлая обработка отделена от HTTP API.

### Этап 6. Widget API

- [ ] Middleware одноразового JWT.
- [ ] Replay-защита `jti`.
- [ ] Tenant context.
- [ ] `/widget/bootstrap`.
- [ ] `/widget/actions/*`.
- [ ] `/jobs/{id}`.
- [ ] Idempotency-Key.

**Результат:** JS-виджет безопасно вызывает backend.

### Этап 7. Workflow

- [ ] Registry обработчиков.
- [ ] Идемпотентность.
- [ ] Защита от циклов.
- [ ] Дочитывание актуальной сущности.
- [ ] Аудит операций.
- [ ] Компенсирующие действия, где нужны.

**Результат:** можно добавлять новый функционал без разрастания handler-ов.

### Этап 8. Production hardening

- [ ] DB roles.
- [ ] KMS/Secret Manager.
- [ ] Metrics и dashboards.
- [ ] Alerts.
- [ ] Backup/restore тест.
- [ ] Retention PII.
- [ ] Load testing.
- [ ] Security review.
- [ ] Runbooks.

**Результат:** платформа готова к production-трафику.

---

## 22. Критерии готовности MVP

MVP считается готовым, когда:

- [ ] новый аккаунт проходит OAuth-установку;
- [ ] токены зашифрованы;
- [ ] одновременный refresh не ломает credentials;
- [ ] Webhook автоматически регистрируется;
- [ ] Webhook endpoint отвечает быстрее двух секунд;
- [ ] событие сохраняется до ответа `2xx`;
- [ ] повторная доставка не запускает повторный side effect;
- [ ] Worker переживает перезапуск;
- [ ] `429` и `5xx` обрабатываются retry-механизмом;
- [ ] виджет может создать job и получить результат;
- [ ] токены и персональные данные отсутствуют в логах;
- [ ] есть метрики, алерты и аудит ошибок.

---

## 23. Что не нужно делать в первой версии

- отдельный `auth-service`;
- собственный балансировщик на Go;
- Kafka только ради названия «микросервисы»;
- service mesh;
- отдельный репозиторий для каждого бинарника;
- Kubernetes до появления реальной потребности;
- синхронную бизнес-обработку внутри Webhook endpoint;
- прямые запросы JS-виджета к amoCRM API в цикле;
- хранение refresh token в браузере;
- десятки мелких сервисов вокруг одной базы.

---

## 24. Когда выделять новые микросервисы

### `auth-service`

Выделять, когда авторизация становится общей для нескольких продуктов, появляется SSO, организации, команды и сложная ролевая модель.

### `amocrm-gateway`

Выделять, когда несколько сервисов независимо вызывают amoCRM и требуется единая точка для токенов, rate limits и API-аудита.

### `webhook-ingress`

Выделять, когда входящий поток Webhooks становится настолько большим, что его нужно масштабировать и релизить независимо от пользовательского API.

### `workflow-service`

Выделять, когда workflow становится самостоятельным продуктовым доменом с собственными версиями, DSL и большим количеством обработчиков.

---

## 25. Итоговая рекомендуемая конфигурация

```text
Публичный слой:
  Traefik/Nginx
  amocrm-api

Фоновый слой:
  amocrm-worker

Данные:
  PostgreSQL
  Redis при горизонтальном масштабировании

Главный поток Webhook:
  amoCRM
    -> HTTPS endpoint
    -> durable inbox в PostgreSQL
    -> HTTP 204
    -> job
    -> Worker
    -> amoCRM API / внешний сервис
```

Эта схема достаточно небольшая для первой версии, но уже закрывает самые опасные места интеграции: быстрый приём Webhooks, одноразовый refresh token, дедупликацию, retries, tenant isolation и независимое масштабирование API/Worker.

---

## 26. Официальная документация amoCRM

- OAuth 2.0: https://www.amocrm.ru/developers/content/oauth/oauth
- Пошаговый OAuth и refresh token: https://www.amocrm.ru/developers/content/oauth/step-by-step
- Одноразовые JWT виджета: https://www.amocrm.ru/developers/content/oauth/disposable-tokens
- Управление Webhooks через API: https://www.amocrm.ru/developers/content/crm_platform/webhooks-api
- Формат и доставка Webhooks: https://www.amocrm.ru/developers/content/crm_platform/webhooks-format
- Ограничения API: https://www.amocrm.ru/developers/content/api/recommendations
