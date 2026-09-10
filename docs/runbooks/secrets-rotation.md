# Ротация секретов и mTLS identities

Процедуры для текущего single-host backend. Шифрование — AES-256-GCM keyring
в `ENCRYPTION_KEYS` / `ACTIVE_ENCRYPTION_KEY_VERSION`. Клиент KMS **не
реализован**. Целевое production-хранилище секретов не выбрано: нет доступа
к целевому серверу. Development Compose и публичный development key не являются
окончательной схемой. Не используйте `cmd/service-certs` и его 30-дневные
localhost-сертификаты в production.

Секреты, ciphertext, JWT, OAuth tokens, webhook keys и тела примечаний в эти
примеры не входят.

## Encryption keyring

Keyring: `version:base64,version:base64`. Каждая версия — ровно 32 байта.
`ParseKeyRing` требует, чтобы активная версия присутствовала в кольце. Open
использует `key_version` рядом с ciphertext; Seal пишет активной версией.
Старые версии обязаны оставаться в `ENCRYPTION_KEYS`, пока существует
ciphertext с этим номером. Удаление версии до перешифрования даёт
`unknown encryption key version` и потерю decryptability.

Хранилища ciphertext:

| Таблица | Колонки | AAD |
| --- | --- | --- |
| `integrations` | `client_secret_ciphertext`, `client_secret_key_version` | `amocrm:integration:{id}:client-secret:v1` |
| `oauth_credentials` | access/refresh ciphertext, `key_version` | `amocrm:installation:{id}:oauth-credentials:v1` |
| `installations` | `webhook_key_ciphertext`, `webhook_key_key_version` | `amocrm:installation:{id}:webhook-key:v1` |
| `installation_webhook_destinations` | `destination_ciphertext`, `key_version` | `installation:{id}:webhook-destination:{hex SHA-256 URL}` |

Автоматической job перешифрования нет. Новые Seal (rotate-secret, OAuth
save/refresh, первичная генерация webhook key) используют активную версию.
Повторная OAuth-авторизация **не** переписывает уже существующий webhook key.

### 1. Добавить ключ

1. Сгенерировать 32 случайных байта, strict standard base64.
2. Дописать `N:<base64>` в `ENCRYPTION_KEYS`, не удаляя предыдущие версии.
3. Выставить `ACTIVE_ENCRYPTION_KEY_VERSION=N`.
4. Rolling restart API, worker и operator CLI с **одинаковым** кольцом.
5. Проверить `/ready` и одну операцию, которая шифрует заново (rotate-secret
   или token refresh). Старые ciphertext должны продолжать открываться.

### 2. Перешифровать

Пока строки со старой `key_version` остаются, старый ключ нельзя убирать.

- Integration client secret: `integrations rotate-secret` (см. ниже) — Seal
  активной версией, AAD с неизменным integration ID.
- OAuth tokens: штатный refresh или повторная авторизация installation
  перезаписывает `oauth_credentials.key_version`.
- Webhook keys: отдельной команды перешифрования нет. Пока
  `installations.webhook_key_key_version` указывает на старую версию, эта
  версия остаётся в keyring. Не обнуляйте ciphertext вручную.

Инвентаризация (read-only):

```sql
SELECT client_secret_key_version AS version, count(*)
FROM integrations GROUP BY 1 ORDER BY 1;

SELECT key_version AS version, count(*)
FROM oauth_credentials GROUP BY 1 ORDER BY 1;

SELECT webhook_key_key_version AS version, count(*)
FROM installations
WHERE webhook_key_key_version IS NOT NULL
GROUP BY 1 ORDER BY 1;

SELECT key_version AS version, count(*)
FROM installation_webhook_destinations GROUP BY 1 ORDER BY 1;
```

### 3. Вывести старую версию

1. Убедиться, что все четыре запроса не содержат выводимую версию.
2. Убрать её из `ENCRYPTION_KEYS` на всех процессах одновременно.
3. Restart API/worker/CLI. Если Open начал отвечать `unknown encryption key
   version`, вернуть ключ в кольцо и не продолжать.

## Integration client secret

Команда уже есть; секрет только через stdin:

```sh
docker-compose run --rm -T integrations rotate-secret \
  --actor operator@example.org --code widget-a \
  --secret-stdin < /secure/widget-a-new-client-secret
```

Полный контекст: [integrations.md](integrations.md). Это синхронизация нового
секрета amoCRM в backend, не выпуск в amoCRM, не перевыпуск OAuth tokens и не
ротация keyring. Disable integration блокирует rotate-secret. После успеха
новые Seal идут активной версией keyring. Старый amoCRM secret перестаёт
приниматься; незавершённый token refresh с предыдущим secret нужно повторить
после ротации.

Env bootstrap (`AMOCRM_CLIENT_SECRET`) не перезаписывает существующий secret.
После первого создания уберите bootstrap credentials из runtime.

## Webhook keys

Ключ генерируется один раз при первой авторизации installation, хранится как
SHA-256 hash + ciphertext, в URL
`https://<PUBLIC_BASE_URL>/hooks/amocrm/v1/{key}`. Повторная авторизация
существующий ключ не меняет. Operator CLI ротации destination **нет**
(остаток CORE-02). Не вставляйте новый ключ SQL-ом: reconcile и amoCRM
subscription разъедутся.

Пока команды нет:

- ротация encryption version webhook ciphertext невозможна без перешифрования
  — не удаляйте соответствующую версию keyring;
- смена destination (компрометация URL) требует новой реализации: новый ключ,
  hash+ciphertext, `webhook.reconcile`, период двойной подписки, затем отзыв
  старого URL в amoCRM. Не отключайте проверку ключа и не логируйте URL.

Transport errors reconciliation очищаются от URL с ключом (BUG-007). Access
log не пишет path/query webhook.

## mTLS identities

`cmd/service-certs` — только local pilot: 30 дней, DNS `localhost`, SPIFFE
`spiffe://amocrm-pro/{core,gateway,activity,crm-events}`, CA private key не
сохраняется, `delegation.key` только у Gateway. **Не выпускайте production
identities этим бинарём и не продлевайте его TTL как замену CA.**

Production:

1. Выпустить те же четыре identity своим CA (TLS 1.3, client+server auth,
   SPIFFE URI, DNS имён процесса, не `localhost` как единственное имя).
2. CA private key не монтировать в runtime. Verify client cert остаётся
   `RequireAndVerifyClientCert`; `InsecureSkipVerify` не включать.
3. Каждый процесс получает только свой `tls.crt`/`tls.key` и общий `ca.crt`.
4. Ротация: выпустить новые сертификаты с перекрытием `NotAfter` старых;
   заменить файлы identity volume; restart по одному процессу; проверить
   `/ready` и один RPC. Пока старый сертификат валиден, peer verify не
   ослаблять. После истечения старого — удалить только его private key.
5. Смена CA: сначала доверенный новый CA во все trust stores (оба CA в
   `ca.crt` на время перехода), затем новые leaf, затем убрать старый CA.
6. `delegation.key` ротировать отдельно, не копировать в Activity/CRM Events.

Истечение **dev**-сертификатов: остановить dev-стек, удалить только четыре
identity volumes, пересоздать `service-certs`; PostgreSQL volume не трогать.
Это не production-процедура.

## Что не ротировать этим runbook

- Widget disposable JWT: короткоживущие, не хранятся как секреты backend.
- amoCRM user passwords и SDK tokens в браузере.
- Пароли PostgreSQL owner/runtime: отдельная смена ролей кластера; после смены
  обновить только соответствующий DSN.
- Публичный development encryption key вне `APP_ENV=development` API/worker
  отвергают при старте.
