# Provisioning нескольких integrations

Operator CLI работает напрямую с PostgreSQL, использует тот же `ENCRYPTION_KEYS`
и `ACTIVE_ENCRYPTION_KEY_VERSION`, что API/worker, и не поднимает management HTTP.
Доступ к контейнеру и DB credentials предоставляется только операторам. `--actor`
обязателен для аудита; это идентификатор оператора/automation, а не способ его
аутентификации. Каждая успешная операция и audit record фиксируются атомарно.

## Подготовка

```sh
make migrate
docker-compose build integrations
```

В production настройте `APP_ENV=production`, `DATABASE_URL`, `ENCRYPTION_KEYS` и
`ACTIVE_ENCRYPTION_KEY_VERSION` через защищённую runtime-конфигурацию. CLI
применяет те же проверки ключей, что API/worker; публичный development key вне
development запрещён. Миграция 000007 однократно включает `lead-status` у уже
существующих integrations для совместимости. Для новых integrations набор
сервисов задаётся явно; `--services none` означает отсутствие продуктовых прав.

## Создание

Поставьте настоящий client UUID и зарегистрированный HTTPS callback URL. Секрет
поступает только через stdin: не передавайте его аргументом, не используйте
`echo SECRET` в shell history. Пример предполагает файл, созданный secret manager,
с ограниченным доступом; сам файл CLI не создаёт и не удаляет.

```sh
docker-compose run --rm -T integrations create \
  --actor operator@example.org --code widget-a \
  --client-id 11111111-1111-4111-8111-111111111111 \
  --redirect-uri https://backend.example.org/oauth/amocrm/callback \
  --webhook-events add_lead,update_lead,status_lead,delete_lead \
  --services lead-status --secret-stdin < /secure/widget-a-client-secret
```

Для второго виджета повторите команду с другим `--code`, `--client-id` и файлом
секрета. `--services` обязателен (`lead-status` или `none`). CLI не выводит секрет
или ciphertext: результат содержит только integration ID, code, status и action.
Повторный `create` существующего code/client ID завершается ошибкой без изменения
данных. Client ID и code после создания неизменяемы; для новой amoCRM integration
создайте отдельную запись.

## Изменение и сервисы

```sh
docker-compose run --rm integrations update \
  --actor operator@example.org --code widget-a \
  --redirect-uri https://backend.example.org/oauth/amocrm/callback \
  --webhook-events add_lead,status_lead

docker-compose run --rm integrations set-service \
  --actor operator@example.org --code widget-a \
  --service lead-status --enabled false

docker-compose run --rm integrations set-service \
  --actor operator@example.org --code widget-a \
  --service lead-status --enabled true
```

`update` меняет только указанные поля. `--webhook-events ''` очищает список.
Изменения webhook events задают intent для следующих OAuth authorizations;
существующие installations получают новый intent при повторной авторизации.
Автоматическое удаление старых remote webhook subscriptions этим CLI не
реализовано. У service grant сохраняется существующий config; CLI управляет
только `enabled`. Поддерживаются только сервисы compile-time каталога.

## Ротация client secret

Получите новый secret в amoCRM и синхронизируйте момент его применения:

```sh
docker-compose run --rm -T integrations rotate-secret \
  --actor operator@example.org --code widget-a \
  --secret-stdin < /secure/widget-a-new-client-secret
```

Шифрование использует активный keyring version и AAD с неизменным integration ID.
Ротация не включает отключённую integration и не затрагивает другие integrations.
Это замена amoCRM client secret в backend, а не операция его выпуска в amoCRM,
не перевыпуск уже выданных OAuth tokens и не ротация ключей шифрования всего vault.
Удаляйте старые encryption keys только после отдельной миграции всех ciphertext.

## Отключение и восстановление

```sh
docker-compose run --rm integrations disable \
  --actor operator@example.org --code widget-a

docker-compose run --rm integrations enable \
  --actor operator@example.org --code widget-a
```

Disable запрещает новую авторизацию/исполнение для integration. OAuth callback
повторно проверяет active status перед записью installation. Авторизованные
транзакции и side effects, уже удерживающие блокировки, завершаются до commit
отключения. После commit worker повторно проверяет доступ перед продуктовым
side effect. API отвечает HTTP 403 с `service_not_enabled`; worker завершает
запрещённую задачу постоянной ошибкой `action_not_authorized`.

Enable восстанавливает status integration, сохраняя индивидуальные service grants
и installation statuses. Ранее отменённые/завершённые jobs автоматически не
перезапускаются. Disable не является uninstall в amoCRM, remote token revocation
или удалением подписок. Если credentials уже отозваны в amoCRM, потребуется новая
OAuth авторизация.

Env bootstrap (`BOOTSTRAP_INTEGRATION_CODE`/`AMOCRM_*`) оставлен только для первой
инициализации: создаёт отсутствующую integration с `lead-status`, но никогда не
перезаписывает существующий secret, config, status или service grant. После
первого подключения уберите bootstrap credentials из runtime-конфигурации.
Рестарт API не отменяет operator disable или ротацию.

## Проверка и аудит

Через `make db-shell` можно выполнять read-only проверки:

```sql
SELECT i.id, i.code, i.client_id, i.status,
       s.service_code, s.enabled
FROM integrations AS i
LEFT JOIN integration_services AS s ON s.integration_id=i.id
ORDER BY i.code, s.service_code;

SELECT created_at, actor_type, actor_id, action, object_id, metadata
FROM audit_log
WHERE object_type='integration'
ORDER BY id DESC
LIMIT 50;
```

Audit использует `actor_type=operator` (`bootstrap` для первой инициализации),
`object_type=integration`, `object_id=integration UUID`. Metadata содержит названия
изменённых полей, статусы и service grants; секреты, ciphertext и redirect URI
в audit не записываются. CLI выдаёт безопасные ошибки без DB connection string,
SQL details или содержимого stdin. Ошибка commit может означать неопределённый
результат при обрыве соединения: перед повтором проверьте status и audit.
