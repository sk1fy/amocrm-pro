# РС-05: доставка событий и результатов

Дата: 02.10.2026. Core runtime дополнен durable fanout, наблюдениями источника,
доставкой outbox и ограниченным восстановлением. Бизнес-решения, полноценная
очередь и разрешение назначения остаются предметом РС-06. Реальный amoCRM pilot
в этой среде не запускался.

## Владение и маршрутизация

Migration20 фиксирует потребителей при приёме webhook delivery. Для новых
доставок список является явным, включая пустой список; NULL означает
pre-upgrade compatibility для прежнего lead-status. Capability включается
после ingress — уже принятое событие не получает нового потребителя.

Parse атомарно создаёт независимые consumer receipts и distribution jobs.
Существующий synchronous lead-status route выполняется в прежнем inbox
transaction. Distribution source job получает собственную транзакцию и fence.
Ошибка или correlated_effect_id lead-status не подавляет distribution child.
Повтор исходного fingerprint не меняет membership и не сбрасывает blocked
receipt, даже после удаления raw delivery/inbox/tombstone по retention.
Долговечный frozen payload сохраняет только идентификаторы и source evidence;
имена и произвольные custom fields остаются в прежней raw retention границе.

Named consumer IDs: `core-lead-status-v1`, `core-lead-distribution-v1`.
Receiver transport consumer: `teamos-distribution-v1`; events/results имеют
отдельные inbox namespaces. sourceEventId — сохранённый Core normalized webhook Event.ID,
а не идентификатор, предоставленный CRM. Он стабилен в consumer receipt,
но не доказывает CRM exactly-once history. Технические messageId/eventId/fingerprint не
являются бизнес-идентичностью повторного входа в этап.

## Фактический источник

Parser поддерживает add/update/status/responsible/delete lead, nested records,
scalar delete IDs и date_create fallback. Подписка восстанавливается через
существующий webhook.reconcile: settings объединяются с distribution events,
другие settings и не принадлежащие Core destinations сохраняются.

Normalizer читает актуальную сделку через существующий общий CRM client и
лимитер. `observationRevision` выделяется до GET; head account+lead обновляется
только большей revision. GET, начатый раньше и завершившийся позже, не заменяет
более новое наблюдение. Snapshot `observedAt` отражает начало чтения.

`entryFingerprint` и `before` остаются null: timestamp tuple не доказывает
бизнес-вход и не позволяет восстановить старого ответственного/воронку.
`sourceEvidence` хранит nullable ID из payload как утверждение источника.
`after` — текущий GET snapshot, который может отличаться от исторического
момента webhook. Generic update, recovery и совпадение владельца с собственным
PATCH дают `lead.snapshot_reconciled`; это не доказательство авторства и не
новый вход. Dedicated responsible event не создаёт stage entry.

Документированный GET204 означает `absent=true`,
`absenceReason=not_found_or_deleted`, snapshot=null, deleted=false.
`is_deleted=true` с точным lead ID означает explicit deletion:
deleted=true, absent=false, absenceReason=null. Неизвестный404,
403, malformed200 и ошибки OAuth остаются source unavailable; отсутствующая
установка или чужой scope не превращаются в удаление сделки.

## Inbox/outbox

Событие, immutable envelope и completed consumer receipt записываются вместе
после проверки job/installation/type/attempt/locked_by/current lease. Job lock
предшествует receipt lock; binding tuple и current capability проверяются на
commit. Новый обработчик после потерянного ответа читает уже сохранённое
сообщение и не меняет IDs/timestamps.

Результаты используют atomic result+outbox РС-04. Sender доставляет events и
results по scoped signed HTTPS HMAC с новой nonce на попытку, private key grant
и timeout 3s. Redirects запрещены. Только202 с exact messageId, nonzero receiptId,
accepted/duplicate и корректным acceptedAt переводит строку в acknowledged.
Чужой/пустой/слишком большой/многозначный ACK считается неуспешной попыткой.

Одинаковые frozen bytes/messageId повторяются после timeout или перезапуска.
Lease/token fencing не позволяет старому sender изменить строку нового.
Backoff ограничен15m, после20 попыток либо permanent4xx (кроме408/429) остаётся
наблюдаемая blocked строка. Ошибка не уничтожает сообщение. Transport loops
не запускаются при выключенном private distribution component; pending rows
не расходуют бюджет и не превращаются в configuration failures.

Historical result scope сохраняется после revoke/rebind: sender не подменяет
binding новым. Migration19 legacy results без leadId не переписываются.
Receiver разрешает legacy missing lead только из зарегистрированного immutable
mirror metadata; GET operation всегда возвращает leadId. Raw envelope hash
сохраняется, semantic operation после trusted enrichment совпадает с polling.
Первое уведомление само по себе не регистрирует произвольную operation.

## Ограниченное восстановление

Private API описан в `api/distribution-openapi.yaml`: live lead observation,
POST/GET recovery scans, guarded retry и delivery-status. ScanId — durable
идентичность запроса; повтор одинакового window/maxPages возвращает принятие,
изменение параметров с тем же ID даёт409. Retry требует ожидаемые page/item
cursor и записывает audit.

После сбоя владелец TeamOS запускает scan в выбранном UTC окне не раньше
создания текущей binding, максимум24h/20pages/250IDs. Запрос ставит обычный
webhook.reconcile job. Только текущие snapshots, без ретроспективного fanout.
CRM updated_at range может требовать включённой функции API-фильтрации;
отказ не заменяется неограниченным чтением всего аккаунта.

Страница ID фиксируется до чтения её элементов. Один tick ограничен10items
и15s. Outbox элемента и item cursor коммитятся вместе; crash/retry не читает
уже сохранённые элементы повторно. Временная ошибка сохраняет тот же scan,
страницу и cursor, повторяется после30s; после20 ошибок сохраняется blocked.
Current binding/capability проверяются перед чтением и на commit. Page limit
оставляет `scan_page_limit`; даже completed scan сохраняет честный
`historical_transitions_unrecoverable`.

Offset pages в изменяемом updated_at диапазоне не являются snapshot database.
Overlap dedup безопасен, полноту исчезнувших/переместившихся между страницами
сделок и пропущенных исторических переходов гарантировать нельзя. Исторический
GET после uninstall/потери OAuth может быть недоступен: guard РС-04 остаётся
удержанным, PATCH не повторяется. Наблюдение desired/previous owner не заменяет
доказательство результата внешней операции.

## Диагностика и эксплуатация

Prometheus: `amocrm_distribution_delivery_messages{kind,state}`,
`amocrm_distribution_delivery_oldest_seconds{kind}`,
`amocrm_distribution_recovery_gaps{state}`,
`amocrm_distribution_delivery_collection_available`,
`amocrm_distribution_unfinished_operations{state}`,
`amocrm_distribution_consumer_receipts{consumer,state}`.
Labels ограничены transport kind/state; lead/employee/company/raw payload в
metrics отсутствуют. Scoped delivery-status возвращает агрегаты по binding.
Blocked receipts/outboxes/scans сохраняют diagnostic code. Технические ошибки
background tick журналируются фиксированным безопасным code.

Оператор сначала исправляет HTTPS/key grants/OAuth/receiver и сверяет inbox
по исходному messageId. Не удаляет receipt/outbox для повторной отправки.
Для исчерпанной transport строки допустим только управляемый повтор того же
frozen payload в проверенном scope; админский UI и отдельный workflow retry
относятся к РС-09. SQL repair проводится по operational процедуре с audit и
предварительным export, а не автоматическим duplicate-webhook reset.

Rollback20 отвергается при использованных consumer/delivery/recovery identities
или source heads. Empty migration up/down остаётся проверяемым. Durable tables
не участвуют в generic TTL cleanup; raw и старые технические jobs могут быть
очищены, nullable ownership pointers SET NULL сохраняют receipt identity.

## Проверки и трассировка

- РС-05.1.1: webhook/store.go, consumers.go; ingress membership, независимый
  child, failure isolation, retained dedup after raw cleanup.
- РС-05.1.2: parser.go, source_events.go; documented responsible/scalar delete,
  sourceEvidence, ambiguous generic update, fresh GET/absence semantics.
- РС-05.2.1: source_events.go, delivery.go, migration20; atomic event outbox,
  exact ACK, immutable retries, stale lease protection.
- РС-05.2.2: existing operation result outbox и новый sender; historical scope,
  GET operation/mirror pull в Team backend, legacy result compatibility.
- РС-05.3.1: observation revision before GET, head CAS; Team inbox/projection
  independently compares source/version/current observation, без RR cursor.
- РС-05.3.2: recovery.go, distribution_scan.go, delivery_metrics.go; frozen
  page/item cursor, bounded window, current scope gates и visible gaps.

Проверки 02.10.2026:
- `make integration-test` — exit0, migration20 apply/empty rollback/concurrent
  migrators и полный PostgreSQL suite всех прежних Core модулей. Лог:
  `/tmp/rs05-core-full-integration.log`.
- `make test` — exit0, Docker formatting/vet/full race suite и builds,
  image `fef35fd3958b`. Лог: `/tmp/rs05-core-make-test.log`.
- После трёх малых final review исправлений (subscription readiness,
  foreign scan ID denial, invalid durable sender envelope disposition)
  `make distribution-integration-test` — exit0, current source race+PostgreSQL,
  15.081s. Лог: `/tmp/rs05-core-final-distribution-db.log`.
- Current bind-mounted Docker profile: gofmt/vet/race для distribution,
  cmd/worker и runtime API — exit0. Лог:
  `/tmp/rs05-core-current-profile.log`.
- Commit preflight восстановил исходную регистрацию `AssignmentFailure`
  через paired `AssignmentWorker.RegisterJobs` в worker composition.
  Actual registered observer проверен настоящим exhausted-job reaper:
  no-attempt освобождает guard, возможный эффект сохраняет unknown guard,
  result/outbox версии согласованы. Повторный current Docker PostgreSQL/race
  для distribution — exit0, 23.552s;
  `/tmp/rs05-core-commit-registration-db.log`. Повторный current Docker
  vet/race для cmd/worker и distribution — exit0;
  `/tmp/rs05-core-commit-check.log`.
- `git diff --check` — exit0.

Полный suite не повторялся после локальных final review исправлений
и paired job registration: текущие
затронутые пути повторно проверены профильным Docker/PG прогоном. Это явная
граница evidence, а не утверждение о повторном полном baseline.
Промежуточные ошибки compile, enum fixture и старого API path count были
исправлены. Промежуточные failed logs не являются итоговым evidence.
Local PG включает immutable ACK retries/leases, historical scopes, legacy19
wire, composed correlated-effect isolation, raw cleanup replay, GET ordering,
204/403/404, frozen scan page/item recovery, scope revoke, visible page gap,
signed HTTP schema validation и безопасные diagnostic labels.

Первичные источники:
- https://www.amocrm.ru/developers/content/crm_platform/webhooks-api
- https://new.amocrm.ru/developers/content/crm_platform/webhooks-format
- https://www.amocrm.ru/developers/content/crm_platform/leads-api
- https://www.amocrm.ru/developers/content/crm_platform/filters-api
