# Company CRM read connector operations

The connector is disabled when all `COMPANY_CRM_*` variables are unset. It uses the
worker's singleton outbound owner and existing OAuth credentials. Do not increase
worker replicas. No new database or migrations are required.

Enable only after operators verify the company account identity and approve its
existing active non-fixture Core installation and integration. Set all required
variables together and restart the worker:

| Variable | Value |
| --- | --- |
| COMPANY_CRM_HTTPS_ADDRESS | Dedicated private listener host:port |
| COMPANY_CRM_IDENTITY_DIR | Directory containing ca.crt, tls.crt, tls.key |
| COMPANY_CRM_ACCOUNT_ID | Verified numeric rakurscrm account ID |
| COMPANY_CRM_INSTALLATION_ID | Exact authorized installation UUID |
| COMPANY_CRM_INTEGRATION_ID | Exact authorized integration UUID |
| COMPANY_CRM_BINDING_REVISION | Explicit revision changed with scope or mapping |
| COMPANY_CRM_ACCOUNT_FIELD_ID | Optional verified company numeric custom field ID |

Partial configuration and invalid TLS files fail startup. Connector construction
makes no owner DB or CRM requests. An unavailable binding or CRM dependency does
not prevent unrelated worker jobs from starting. The worker still requires its
normal database and other startup dependencies.

Authenticated reads check the owner DB binding before using cached data and
verify provider account identity before scanning. A cold binding or provider
failure returns HTTP 503 with null data and an incomplete error source. Reads
recover after the existing retry interval when the dependency is healthy.
Provider 429 cooldowns start on receipt and apply across methods. Use the response
`X-Request-ID` for correlation. Each read retains its ten-second context.
The server certificate must verify for the hostname callers use. The
existing service certificate tooling retains its CA layout. Provision
separate client-auth certificates through the trusted service identity process
with exactly one URI each, `spiffe://amocrm-pro/admin-company-directory` or
`spiffe://amocrm-pro/platform-company-crm`. The existing TeamOS and Platform
identities are intentionally insufficient. Do not put a plaintext proxy in front
of this listener. Restrict network access to the two callers.

Without the optional account field ID, companies and contacts remain visible with
unknown account resolution. Before setting it, verify numeric field metadata,
exact integer encoding, uniqueness of each customer account across companies and
whether multiple customer accounts belong to one company. Field names are not
configuration evidence. Rebinding requires a new revision and worker restart.
Cached state is memory-only and disappears on restart.

Consumers must read `source.state`, `complete`, account, revision, timestamps and
retry_after. Null data never means an empty directory. A stale complete snapshot
retains its old source identity after a failed binding transition. Do not issue
new staff grants from stale/unknown observations. Missing staff never implies
inactive. The Staff consumer owns staff reconciliation and Admin session policy.
The Clients consumer owns its projection independently of runtime installations.

Workflow validation proves only pipeline-stage membership in the observed source
revision. Required fields are still unknown and `write_ready` remains false.
Deal creation is opt-in. Required stage fields can cause a definitive
rejection that the manager can retry after fixing the configuration.
Deployment and production CRM access are outside the local checks.

## Local certificate and Compose flow

Generate a new local development CA and exact scoped client identities with the
existing Docker target. Use a new output directory:

```sh
make PLATFORM_DEV_CERTS_DIR="$PWD/tmp/company-crm-certs" platform-certs
```

The `--platform` mode creates `core`, `admin-company-directory` and
`platform-company-crm` certificates under the same development CA. The two
company callers have client-auth EKU only. Core includes the `company-crm` DNS
alias. The CA private key is not saved. Copy no development CA to production.
For a local nonroot worker, pass `COMPANY_CRM_UID=$(id -u)` and
`COMPANY_CRM_GID=$(id -g)` to Compose so it can read the generated private key
without changing its permissions. The optional overlay defaults to UID/GID 10001.

After supplying verified binding variables and `COMPANY_CRM_HTTPS_ADDRESS=:8086`,
validate the optional overlay with `docker-compose -f docker-compose.yml -f
docker-compose.company-crm.yml config --quiet`. An explicitly authorized local
start uses the same file pair. The overlay mounts only the Core identity and
publishes port 8086 to loopback. Container callers use `https://company-crm:8086`.
The connector remains disabled with empty binding variables. A synthetic test
fixture is not an approved real credential binding.

A local read with the generated Staff identity uses:

```sh
curl --cacert tmp/company-crm-certs/admin-company-directory/ca.crt \
  --cert tmp/company-crm-certs/admin-company-directory/tls.crt \
  --key tmp/company-crm-certs/admin-company-directory/tls.key \
  https://localhost:8086/v1/company-crm/staff
```

## Подключение отдельного виджета для сделок

Установите приватную интеграцию «Ракурс — кабинет менеджеров» вручную
в аккаунте компании. Зарегистрируйте её Client ID и Client Secret через
существующий OAuth-контур Core с кодом интеграции `rakurscrm-manager`.
После установки сверьте идентификаторы
аккаунта, интеграции и установки. Используйте их в `COMPANY_CRM_*` и
назначьте новую `COMPANY_CRM_BINDING_REVISION`. В Platform укажите тот же
аккаунт и ревизию. В «Установках» откройте «Настройка сделок» и
сохраните общую воронку и начальный этап. Настройки отдельных продуктов
сохраняются в истории. Перед включением обработки заполните общую
настройку для активной привязки.

Для виджета 1.1.5 Core API обслуживает
`GET /api/v1/company-cabinet/activation` с origin аккаунта компании.
Проверьте доступность маршрута после обновления Core. Из карточки можно
запустить существующую авторизацию amoCRM для `rakurscrm-manager`.

`COMPANY_CRM_DEALS_HOST=rakurscrm.amocrm.ru` разрешает запись только через
настроенное подключение этого аккаунта. `PLATFORM_COMPANY_DEALS_ENABLED=true`
передаёт задачи существующему worker Platform. На Core API установите
`PLATFORM_ACTIVATION_ORIGIN` (приватный HTTPS origin Platform) и смонтируйте
каталог `PLATFORM_CORE_IDENTITY_DIR` с ролью Core. Миграция Platform 000013
добавляет данные активации, задачи и повторные попытки сделки. Миграция
Platform 000014 добавляет ревизии общей настройки.

Новые подтверждённые установки получают задачу. Если контакт не введён,
задача ждёт до 24 часов с установки. Для «Запрет работы» и закрытых
эпизодов задача пропускается. Автоматические письма требуют отдельной
настройки и в этой версии не отправляются.

Создайте в rakurscrm текстовое поле сделки «Ракурс — ID установки» и
задайте его ID через `COMPANY_CRM_DEAL_MARKER_FIELD_ID` в worker Core.
Проверьте, что поиск сделок по метке через `query` возвращает это поле. Обновите worker Core с этим
параметром до обновления Platform, которая составляет новые названия.
Название включает виджет, аккаунт, при наличии контакт и дату установки
в часовом поясе кабинета. Поле сделки хранит метку РКРС.

После неоднозначного ответа повтор ищет сделку по точной метке в поле.
Для прежних резервов с меткой в названии используется поиск по названию.
При отсутствии результата задача остаётся на сверке. При явном отклонении
запроса менеджер может повторить создание после исправления причины.
[Решение по активации и сделкам](adr/0050-installation-activation-and-company-deal.md).
