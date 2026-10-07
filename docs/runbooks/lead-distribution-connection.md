# РС-03: подключение распределения

Реализованы подключение и policy; назначение сделок ещё не реализовано.
Core применяет миграцию 000018 обычным owner migrator. Runtime не выполняет DDL.

## Включение

1. Создать/подключить интеграцию через существующие OAuth start/callback.
2. Выдать `lead-distribution` через операторский `integrations set-service`
   (явный integration code, actor, service и enabled). По умолчанию capability нет.
3. Разместить серверные ключи только в deployment secret env:
   `DISTRIBUTION_SERVICE_KEYS` — JSON object key ID → секрет длиной минимум 32 символа.
   `DISTRIBUTION_TEAMOS_KEY_ID` выбирает ключ обратных вызовов.
   `DISTRIBUTION_TEAMOS_URL` — HTTPS origin TeamOS company backend.
   `DISTRIBUTION_HTTP_ADDRESS=:8084`, `DISTRIBUTION_TLS_PROXY=true` включают private
   listener worker; поставить доверенный TLS proxy и network firewall.
   Опциональный `docker-compose.distribution.yml` добавляется к базовому compose;
   он передаёт deployment secrets в worker и не публикует порт на хосте.
4. Через `cmd/distribution-grant` выдать точный key/company/installation grant:
   `--key-id KEY --company-id UUID --installation-id UUID --actor OPERATOR --enabled true`.
   Команда использует operator environment, записывает audit, не принимает секрет.
   Собрать контейнер: `make distribution-grant-build`. Запускать утилиту в
   приватной сети, где доступна operator БД, например:

   ```sh
   docker run --rm --network YOUR_PRIVATE_NETWORK \
     --env DATABASE_URL --env ENCRYPTION_KEYS --env ENCRYPTION_KEY_VERSION \
     --env APP_ENV amocrm-distribution-grant:local \
     --key-id KEY --company-id COMPANY_UUID --installation-id INSTALLATION_UUID \
     --actor OPERATOR --enabled true
   ```

   Значения секретов наследуются из защищённого operator environment, в аргументы
   не помещаются. Использовать owner/operator DSN с нужными правами на grant/audit;
   standalone продуктам Core DSN не выдавать.
5. В TeamOS owner/admin создаёт intent и подтверждает его свежим одноразовым
   widget JWT администратора amoCRM. Произвольные browser IDs не являются полномочиями.
6. Импортировать полный live снимок users/pipelines и сопоставить прежние employee UUID.
   Отправить mappings с новой mappingRevision. Не создавать сотрудников по имени/email.

## Runtime HTTP

Все service endpoints находятся на private listener worker:

- POST `/internal/v1/distribution/bindings`: companyId, bindingId,
  bindingRevision=1, installationId, integrationId, accountId (decimal string),
  widgetToken, intentId, expiresAt (RFC3339 UTC; максимум 15 минут).
- GET `/internal/v1/distribution/bindings/{bindingId}`: восстановление потерянного ACK.
- GET `.../{bindingId}/readiness`: OAuth/subscription/capability states, recoveryAction; assignmentReady=false до последующих этапов.
- POST `.../{bindingId}/revoke`: отмена с tombstone; допускается до confirm.
- GET `.../{bindingId}/references`: users и pipelines/statuses — полный ограниченный
  live snapshot, fetchedAt/freshUntil, state=fresh. На source failure — 503, демоданных нет.
  Users/subscriptions ограничены четырьмя страницами по 250; pipelines четырьмя по 50.
  С РС-11 также возвращаются `timezone` и `timezoneFetchedAt`: часовой пояс из
  `/api/v4/account?with=datetime_settings` после проверки ID аккаунта привязки и
  IANA timezone. Пустое/неизвестное значение и `Local` дают 503; UTC не подставляется.
  `accountDomain` — канонический hostname той же подтверждённой установки,
  проверенный по allowlist amoCRM/Kommo; для ссылок на сделки TeamOS использует его
  только после положительной проверки прав. Дополнительных CRM запросов для
  получения домена каждой сделки не требуется.
  TeamOS хранит последнее подтверждённое значение отдельно для каждой привязки.
  Отсутствие этих полей у старой версии Core не подтверждает ручную настройку как
  часовой пояс amoCRM. Временный сбой источника не должен стирать подтверждённый кэш.
- PUT `.../{bindingId}/mappings`: mappingRevision и mappings[{employeeId,userId}].
  Same revision+same hash возвращает успех; старый или конфликтующий snapshot — 409.
- POST `.../{bindingId}/permissions`: employeeId, userId, leadId.
  CRM IDs — decimal strings. Возвращает userId, canViewLead, reason, checkedAt.

Каждый запрос содержит X-Distribution-Key-Id/Company/Installation/Timestamp/Nonce/
Signature. Signature — lowercase hex HMAC-SHA256 над строками, соединёнными LF,
**без завершающего LF**: keyId, method, RequestURI, company UUID, installation UUID,
Unix timestamp seconds, nonce UUID, lowercase hex SHA256 exact body bytes.
Все заголовки одиночные, body <=64KiB. После 429/503 делать backoff; новый transport
nonce и исходный intent/binding ID. После uncertain mutation читать GET, не создавать
новую привязку автоматически.

Public widget routes через явно настроенный ingress proxy:
GET `/api/v1/widget/distribution/bootstrap`, POST `.../permissions` с `{leadId:"123"}`.
Проверка widget JWT/CORS использует существующий tenant/issuer механизм,
те же WIDGET_JWT_LEEWAY/MAX_LIFETIME настройки и bounds, JTI read
consumption, отдельный ограниченный admission limiter. Bootstrap не возвращает
сотрудников и полные CRM rights. Для обоих методов Core вызывает подписанный
TeamOS `/internal/v1/distribution/widget-access`; TeamOS отвечает current employee ID
и section decision, которые сверяются с mirror. Неопределённая policy — 503.

## Восстановление и отключение

Reauth_required: выполнить существующий OAuth flow той же integration/account,
employee/binding UUID не менять. Disabled/uninstalled installation или отключённая
integration/capability прекращают новые product calls. Историческая привязка
сохраняется для восстановления/диагностики. Explicit revoke отменяет связь,
последующая новая связь получает новый binding ID и новую проверку обеих сторон.
Сначала revoke прежний intent, затем создавать новый; tombstone блокирует late confirm.

Ротация: добавить новый key в обе системы, выдать ему точные scoped grants,
переключить outbound key ID, проверить чтение, отключить старые grants и удалить
старый secret. Нельзя копировать admin API token или OAuth credentials в bridge.

Локальные проверки: `make distribution-test`, `make integration-test`, `make test`.
Live OAuth reauthorization, installed widget JWT и deployment TLS evidence на
тестовом аккаунте здесь не выполнены: требуются доступные аккаунт и окружение.

## РС-04: private operation API и восстановление

Текущий реализованный контракт: `api/distribution-openapi.yaml`. POST
`/internal/v1/distribution/assignments` принимает AssignmentEnvelope с canonical
UUID Idempotency-Key и возвращает **202 OperationReceipt** после durable commit.
GET `/internal/v1/distribution/operations/{operationId}` возвращает Operation.
POST `.../{operationId}/cancel` и `.../{operationId}/reconcile` принимают
`{expectedResultVersion, actor, reason}` плюс UUID Idempotency-Key и возвращают
**200 Operation**. Время запроса сохраняет сервер. Retry использует тот же
operation/payload/key, новый только transport nonce. Чужие IDs не раскрываются.

Перед production отправкой требуется TeamOS подписанный `validate-decision` с
capability **decision-validation** (widget-access grant не подходит). Текущий seam
проверяет scope/mapping/actor, но возвращает decision_not_ready до РС-06. Не обходить
его синтетическим разрешением и не выдавать себя за существующий business registry.
Подробности Core реализации и локальной приёмки — в спецификации 08.

outcome_unknown означает, что PATCH мог дойти; guard account+lead не имеет TTL.
Смена binding, рестарт worker, cancel, pause, matching GET или другой installation
не разрешают повтор. Reconcile делает GET и добавляет observation; terminal успех
возможен только по уже сохранённому valid ACK плюс новому GET после ACK с достаточно
свежим source updated_at. Потерянный ACK, 5xx, transport timeout и malformed response
остаются unknown. HTTP 401/403/404/429 тоже не служат автоматическим release proof;
причина сохранена отдельно, PATCH повторять нельзя.

После revoke/capoff исторический GET/cancel/reconcile остаётся доступным scoped
service principal. При недоступном OAuth после disable/uninstall/reauth вернуть
подключение штатным способом и повторить только GET/reconcile; не создавать новый
operation. OAuth refresh с потерянным исходом восстанавливается существующим
OAuth protocol. Публичного operator resolve с self-reported proof нет: проверяемое
внешнее evidence требует будущего операторского механизма, а неизвестный guard
до этого сохраняется. Не выполнять SQL DELETE guard/receipts/jobs как «исправление».

Каждый resultVersion имеет атомарный outbox; отправка результатов будет реализована
в РС-05. До этого проверять GET operation. Journals/jobs pinned, семидневный cleanup
их не удаляет. Rollback migration19 с использованными operations запрещён.
