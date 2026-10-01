# РС-02.2. API и межсервисная доставка

Дата: 2026-10-01. Статус: proposed contract для разработки. Runtime endpoints,
миграции и обработчики этим пакетом не добавлены. Критерии РС-02.2.1 и
РС-02.2.2 раскрыты в машиночитаемых схемах, правилах совместимости и fixtures.

Основа: [правила](01-business-rules.md),
[владение и права](02-architecture-and-access.md),
[модель и lifecycle](04-data-model-and-lifecycle.md).
Интерфейсные сценарии используют тот же контракт:
[макеты](06-interface-scenarios.md).

## 1. Артефакты и положение относительно canonical API

| Артефакт | Назначение |
| --- | --- |
| [TeamOS OpenAPI](contracts/teamos-distribution.openapi.yaml) | Новые пользовательские пути, DTO, пагинация, фильтры, ошибки |
| [Core OpenAPI](contracts/core-distribution.openapi.yaml) | Bridge двух backend, scoped виджетный фасад и техническая диагностика |
| [Protobuf](contracts/distribution.proto) | Типизированные DTO и RPC company/Core; frontend wire names определяет REST-адаптер |
| [Fixtures](contracts/fixtures/lead-distribution/schema-cases.json) | Положительные и отрицательные payload без production-данных |
| [Проверка контрактов](contracts/fixtures/lead-distribution/verify-contracts.mjs) | Компиляция schemas, semantic assertions, route invariants и соответствие enum с proto |

Это отдельный proposed пакет внутри спецификации, не второй runtime source
of truth. Причина: текущий Core OpenAPI описывает смонтированные handlers;
`api/openapi_test.go` сравнивает его с `internal/apicontract/routes.go`.
Добавление несуществующих маршрутов в canonical нарушило бы этот инвариант.
TeamOS canonical OpenAPI также генерирует gateway, а его proto — реальные
interfaces. Поэтому здесь конкретно спроектированы обновления контрактов,
но canonical generation выполняется вместе с реализацией handlers на следующих
этапах. Нельзя выдавать текущую проверку schemas за реализованное расширение API.

При реализации:

1. Перенести совместимые схемы/пути TeamOS в
   `team-os-backend/contracts/openapi/teamos.yaml`, RPC/поля в
   `contracts/proto/company/v1/company.proto`. Использовать его existing
   gateway actor metadata, не вводить доверие к произвольному `UserContext`.
2. Сохранить существующие номера proto и names; новые добавить. Предложенный
   пакет `rakurs.distribution.v1` — граница DTO, не инструкция заменить
   существующий `company.v1` или Activity RPC.
3. Обновить gateway/application/storage в одной вертикальной части, затем
   `make gen`, `make check-contract` и профильные тесты TeamOS backend.
   Обновить frontend через `npm run contract:generate`, проверить
   `npm run contract:check`. Generated файлы вручную не редактировать.
4. Core paths внедрить в `api/openapi.yaml` / `api/admin-openapi.yaml` и route
   registry одновременно с handlers. Межсервисный port — transport-neutral
   `internal/serviceapi`, protobuf adapter — transport слой.
5. Новые права/наблюдения админки внести в её OpenAPI, RBAC и
   `docs/design/{roles,states,data-sources}.md`; runtime изменения проверить
   по её STYLE_GUIDE. До этого proposed admin schemas не означают новый экран.

## 2. Общие типы и совместимость

- Public TeamOS/widget JSON — camelCase. Admin projection сохраняет
  snake_case и `observed_at`; адаптер не переименовывает backend states молча.
- Platform IDs — UUID. CRM `accountId/leadId/pipelineId/statusId/crmUserId`
  — положительные decimal strings без ведущих нулей, значение не больше
  `9223372036854775807`. Числовой верхний предел проверяется приложением,
  regex сам его не доказывает. JS не переводит CRM ID через `Number`.
- `revision`, `availabilityRevision`, `claimRevision`, `resultVersion` —
  integer `1..9007199254740991`. Proto `uint64` и DB bigint имеют тот же check.
- Время — RFC3339 UTC; графики используют IANA timezone компании. Время
  получения не подменяет неизвестное время события: `sourceOccurredAt=null`.
- Отсутствие наблюдения/ответственного — `null`, не `0`, пустая строка или
  «никто». `freshness=unknown/unavailable` не является пустым списком.
- `allowedActions` — подсказка UI, каждый command повторно проходит server
  checks. Новый/неизвестный enum не трактуется как разрешение или успех.
- SchemaVersion 1 расширяется совместимыми необязательными полями. Клиенты
  игнорируют неизвестные response fields; входящие команды проверяются
  строго. Удаление/переопределение поля требует новой версии.

Старые `GET /api/v1/distribution/groups` и `/groups/{id}/events` сохраняют
массивы и прежние типы. Для пагинации вводится новая `/groups/overview`.
`DistributionEvent.dealNumber` не становится CRM ID. Старые
`accepted/in_progress/reassigned/declined` не подменяются queue states.
Новый rule v1 допускает **только round_robin**; legacy least_loaded/priority
требует явного переключения, а не silent fallback.

### Группа и CAS правила

`CreateRuleInput` связывает существующую группу с правилом. Сервер сравнивает
`expectedGroupUpdatedAt` с owner row; значение доступно в
`GroupOverview.groupUpdatedAt`. Атомарно сохраняются группа, упорядоченные
участники и первая rule revision. `PUT /rules/{ruleId}` меняет эти данные
в одной транзакции с `expectedRevision`; отдельного владельца членства нет.

После создания managed rule — включая draft и paused — старые PATCH/DELETE
этой группы должны отвечать `409 distribution_rule_managed` с предложением
открыть новые настройки. Несвязанные legacy группы продолжают прежний путь.
Это явное ограничение для новых managed объектов, не обещание абсолютной
поведенческой совместимости старого клиента. Поэтому activation запрещена,
пока все writers не знают guard. Обновления графиков/пользователей отдельно
вызывают атомарную invalidation доступности, как описано в модели РС-02.1.

`enabled=true` соответствует active; false при создании — draft, при
изменении — paused. GET Rule возвращает явный state. Archived rule не
редактируется. Смена source/binding с активными или uncertain операциями
отклоняется до их сверки; PUT не перепривязывает уже принятую команду.

## 3. Пользовательские endpoints

Общий префикс TeamOS: `/api/v1/distribution`.

| Метод и suffix | Результат и существенные проверки |
| --- | --- |
| GET `/connections`, `/connections/{bindingId}` | Подключения только текущей компании, состояние и freshness |
| POST `/connections/link-intents` | Одноразовое намерение owner/admin, 201; account сверяется с компанией; не активация сама по себе |
| GET `/references/users?bindingId=…` | Разрешённый справочник, mapping, availableNow, следующая смена |
| GET `/references/pipelines?bindingId=…` | Справочник воронок с этапами проверенного account |
| GET `/groups/overview` | Пагинируемая проекция существующих групп, состояние и доступные actor агрегаты |
| GET/POST `/rules` | Чтение / создание с idempotency и проверкой группы, binding, сотрудников, алгоритма |
| GET/PUT `/rules/{ruleId}` | Версия и атомарная CAS-конфигурация |
| GET `/queue`, `/queue/{episodeId}` | Только разрешённые сделки; детали содержат первые 100 событий и historyNextCursor |
| GET `/history` | История решений, фильтр episode/group и интервал |
| GET `/stats?from=…&to=…` | Показатели по разрешённым actor ресурсам, максимум 31 день |
| POST `/rules/{ruleId}/commands` | pause/resume/recalculate, expectedRevision и Idempotency-Key |
| POST `/queue/{episodeId}/commands` | cancel/reconcile/retry, expectedRevision и проверка текущего эффекта |
| GET `/commands/{commandId}` | Восстановление результата только исходным actor/tenant; episode дополнительно CanViewLead |

Просмотр разрешён employee с доступом к разделу и `CanViewLead=allow` для
конкретной сделки; owner/admin не обходит CRM resource ACL. Управление
правилами/очередью требует owner/admin TeamOS. Company выводится из verified
session; идентификаторы тела и пути только selectors. Другой tenant получает
404 без подтверждения существования объекта.

### Пагинация и фильтры

Cursor opaque, подписан/проверен сервером и связан с actor, компанией,
правами, фильтрами, сортировкой и версией запроса. `limit=25`, максимум 100.
`nextCursor=null` завершает выборку. Изменение фильтра сбрасывает cursor.
Просроченный/неподходящий cursor — 400 `cursor_expired/cursor_invalid`;
клиент перезапрашивает первую страницу, сохраняя фильтры.

Queue фильтруется по group/rule/binding/lead ID, набору states, receivedAt
интервалу `[from,to)`. Дефолт `receivedAt.asc`, устойчивый tie-breaker
`episodeId.asc`; для desc оба ключа убывают. History — `at.desc,historyId.desc`.
Группы/правила — `createdAt.asc,id.asc`; справочники — CRM numeric ID asc,
при этом wire ID остаётся строкой. Одна страница не обещает snapshot всей
изменяющейся очереди; обновлённые строки могут требовать refresh первой страницы.

Сначала применяется tenant/resource policy, затем страница и counts.
Недоступные сделки не раскрываются через total, имя, history, поиск или
порядок cursor. В ответах нет глобального total. При недоступной policy —
503, а не ложный нулевой результат.

### Смысл управления и показателей

- pause сохраняет очередь, исходную причину `pausedReason`, следующий
  плановый момент; это время advisory и не запускает работу при паузе.
- resume и recalculate дают новый расчёт из текущего графика/правила, а не
  отправляют старый plannedResponsible. Planned всегда `isTentative=true`.
- cancel фиксирует запрос; после возможного PATCH не объявляет cancelled
  и не освобождает guard. UI показывает outcome_unknown до доказательств.
- reconcile делает чтение/сверку. retry допускается после разрешённого
  внешнего исхода и создаёт новую decision; unknown нельзя слепо повторить.
- `assignedCount` — подтверждённые ходы очереди, включая already_target;
  `unchangedCount` — already_target; `actualChangedCount` — доказанные
  фактические смены ответственного. `keptCount` не расходует ход очереди.

## 4. Виджет и админка

Для виджета Core публикует те же suffix под
`/api/v1/widget/distribution/`, кроме создания TeamOS link intent.
Пути и response schemas перечислены в Core OpenAPI. Проверка disposable
amoCRM JWT устанавливает account/client/user; Core выводит binding/TeamOS
actor на сервере. Browser company/actor headers не являются authority.
Ролевое пересечение и `CanViewLead` остаются такими же, как в TeamOS.
Первичная CRM-сторона binding подтверждается server-to-server после live
проверки CRM admin; endpoint связывания не выдаёт OAuth браузеру.

Admin API обращается к внутренним Core endpoints:

- GET `/admin/v1/installations/{installationId}/distribution` — безопасная
  диагностика, last event/delivery, pending/unknown counts, observation.
- POST `…/distribution/commands` — pause_module/resume_module,
  reconcile_subscription/retry_delivery/reconcile_operation.
- GET `…/distribution/commands/{commandId}` — scoped receipt.

Viewer только читает; operator/admin выполняет разрешённое конкретное
техническое действие по RBAC. Изменение capability всей интеграции остаётся
admin-only. Existing Core admin Bearer принадлежит Admin API, не браузеру;
`X-Admin-Actor` аудируется и не аутентифицирует сам по себе. GET receipt
проверяет actor scope, а доступ диагностики других операций требует явного
permissions пути. Никаких direct DB/CLI из web handler.

## 5. Межсервисные endpoints и envelope

| Endpoint | Получатель и смысл |
| --- | --- |
| POST `/internal/v1/distribution/events` | TeamOS company: durable event inbox |
| POST `/internal/v1/distribution/results` | TeamOS company: durable result inbox |
| POST `/internal/v1/distribution/bindings/confirm` | TeamOS company: одноразовый intent и подтверждённая CRM identity |
| POST `/internal/v1/distribution/assignments` | Core: операция + job + receipt в одной транзакции |
| GET `/internal/v1/distribution/operations/{operationId}` | Core: сохранённое актуальное состояние, без побочного PATCH |
| POST `…/operations/{operationId}/reconcile` | Core: ограниченная сверка с expectedResultVersion |
| POST `…/operations/{operationId}/cancel` | Core: durable cancel request; не обещание отмены отправленного запроса |
| POST `/internal/v1/distribution/decisions/{decisionId}/validate` | TeamOS company: свежая проверка решения перед конкретной попыткой эффекта |

Контракт требует mTLS identity + signed delegation с issuer/audience/action
и scope. Новые grants самостоятельны: не выдавать себя за Activity. Role
пользователя и service principal раздельны. Контракт проверяется одинаково
в HTTP/RPC-адаптере и прикладном слое; transport не обход проверки.

`EventEnvelope`, `AssignmentEnvelope`, `ResultEnvelope` содержат schemaVersion,
messageId, eventId, исходный sourceEventId при наличии, sourceOccurredAt,
receivedAt, emittedAt, correlationId, causationId и полный scope:
company/account/integration/installation/binding/bindingRevision.
Body scope сверяется с authenticated identity и сохранённой привязкой,
а не принимается как разрешение.

Команда дополнительно содержит operation/episode/decision/group/rule ID,
ruleRevision, availabilityRevision, claimRevision, decisionKind assign/keep,
targetResponsibleUserId, expectedSnapshot, verified actor и validUntil.
`keep` не делает PATCH; target должен совпасть с наблюдаемым ответственным.
Истёкший validUntil запрещает новую отправку, но не доказывает, что предыдущая
не дошла. Lead scope всегда account+lead, а не только installation+lead.

### Свежесть решения и отмена

После ожидания исходящего бюджета и свежего чтения сделки Core вызывает
ValidateDecision перед переходом к applying. Company проверяет активность
binding/rule, episode, неизменность всех revisions, mapping, график,
получателя, отсутствие cancel/pause и текущий claim. Reply привязан к
operationId/decisionId/workerFence, действителен максимум 5 секунд и не
кешируется между попытками. Timeout или unavailable — эффекта нет.

Это короткий снимок разрешения, не распределённая SQL-блокировка. Изменение
после успешной проверки может совпасть с уже начатой отправкой; pause/cancel
в этом окне становятся cancellation_requested, и результат сверяется.
Нельзя обещать атомарность изменения графика с внешним PATCH. Core повторно
проверяет свой capability и fence при фиксации dispatch intent; потерянный
lease не даёт новому worker повторить неизвестный внешний эффект.

Отзыв binding запрещает новое назначение, но cleanup/result/reconcile
разрешаются узкому principal для immutable scope ранее принятой operation.
Иначе старый uncertain guard нельзя было бы безопасно разрешить. Это не
разрешение операции нового binding или отключённого модуля.

## 6. Идемпотентность, версии результата и восстановление

Все изменяющие запросы требуют Idempotency-Key UUID. Для bridge доставка
использует стабильные messageId/operationId, новые nonce/auth токены не меняют
семантику повтора. Новый бизнес-запуск — новый episode/decision/operation,
а transport retry сохраняет исходные идентификаторы.

Ключ lookup receipt: stable bindingId + action + idempotency key;
пользовательские команды дополнительно actor, первичный intent — company+actor
до появления binding. Consumer inbox отдельно дедуплицирует consumer+source+messageId.
BindingRevision/installation/account не создают новое key namespace: они входят
в immutable payload hash. Тот же ключ с новой revision или installation даёт
409, а не новое назначение. Отдельные UNIQUE operation ID
и payload hash исключают повтор с другим transport key. Hash вычисляется от
типизированного канонического payload: fixed field order, canonical UUID/CRM
IDs, UTC timestamps, явные nulls, set arrays нормализованы; **memberIds сохраняет
порядок**. Auth/delegation, traceId, transport attempt headers и transport retry timestamp
не входят в hash. SourceOccurredAt/receivedAt/emittedAt фиксируются при первом
сохранении envelope и остаются неизменными при повторе; они входят в semantic
hash. CorrelationId — стабильная бизнес-корреляция, transport traceId — отдельная
переменная диагностики, не заменяющая её.

| Ситуация | Поведение |
| --- | --- |
| Первый запрос принят | Commit receipt/inbox/job до 202 |
| Тот же key + payload | Тот же durable receipt/operation ID; без второго эффекта |
| Тот же key или operation ID, другой payload/scope | 409 idempotency_conflict; неизвестный tenant не раскрывается |
| Нет ответа POST | Повторить исходный ключ либо GET известной операции; новый ID запрещён |
| Receipt queued | Только принятие; UI не пишет «назначено» |
| Потерян result push | GET operation тем же scoped principal; latest resultVersion; polling с backoff |
| Результат с меньшей версией | ACK/idempotent ignore, состояние не регрессирует |
| Та же resultVersion и тот же hash | Duplicate ACK |
| Та же resultVersion и другой hash | 409 protocol/idempotency conflict; сохранить диагностику |

Core outbox результата фиксируется атомарно с operation result. TeamOS
принимает push своим inbox, затем атомарно применяет result/version и cursor
round-robin. ACK означает commit входящей доставки, не окончание CRM-записи.
Retentions и tombstones не короче срока повтора/ручного восстановления;
конкретные параметры — по [модели](04-data-model-and-lifecycle.md).

Старый bindingRevision результата сверяется с immutable scope его operation.
Он может завершить старое решение и снять **его** guard при evidence, даже
если current binding уже изменён. Он не актуализирует current binding и не
завершает другую операцию. Envelope/result scope, eventId и correlationId
должны совпасть. Повторный вход сделки на этап не изменяет старую operation.

Один webhook fanout создаёт независимые consumer deliveries; сбой distribution
не отменяет lead-status. Consumer ID содержит версию бизнес-контракта, не имя
worker. Новый consumer не является основанием повторить исторический эффект.

## 7. Состояния, доказательства и ошибки

Wire enums в OpenAPI и protobuf автоматически сверяются проверкой fixtures.
Queue и Core operation имеют разные состояния; Core jobs остаются технической
реализацией. `externalEffectState=settled` не означает успех само по себе:
смысл задаёт `outcome` и evidence.

- Все terminal состояния требуют guardReleasable=true и evidence, отличное
  от observed_state_only. succeeded требует response_and_observation либо
  проверенной operator_reconciliation; no_request_sent для него запрещено.
- succeeded → outcome assigned, confirmedSnapshot и settled.
- no_change → outcome kept либо already_target и confirmedSnapshot; kept
  сохраняет ход, already_target учитывает выбранный ход один раз. Для no_change
  evidence=no_request_sent: цель совпала до попытки внешней записи.
- rejected/cancelled/conflict допустимы только при no_attempt/settled.
  Возможный поздний PATCH оставляет confirming/outcome_unknown.
- Наблюдение GET/webhook без доказанного завершения запроса имеет
  evidence=observed_state_only и `guardReleasable=false`, даже если target
  совпал. Оно не доказывает авторство или отсутствие поздней записи.
- error.retryable и error.terminal — разные флаги. Retryable не разрешает
  повтор PATCH, пока external effect unknown; terminal не отменяет необходимость
  evidence. Новая попытка business decision разрешается владельцем очереди.

TeamOS envelope ошибки сохраняет `{error:{message,status}}`; code, requestId,
details добавлены. Тексты русские, безопасные. Core admin сохраняет свой
`{error:{code,message,request_id,retryable}}` без смены формы.

| HTTP | Code / действие клиента |
| --- | --- |
| 400 | validation_failed, unsupported_algorithm, cursor_invalid/expired; исправить ввод или начать новую страницу |
| 401 | unauthenticated; обновить пользовательский вход, не повторять старый disposable JWT |
| 403 | permission_denied, capability_revoked; не повторять без изменения прав |
| 404 | resource_not_found; включает чужой tenant без раскрытия объекта |
| 409 | revision_conflict, idempotency_conflict, binding_mismatch, stage_already_owned, operation_unresolved, invalid_transition, distribution_rule_managed; получить актуальное состояние, не перезаписывать молча |
| 409 | binding_not_active, mapping_required, reauth_required; устранить причину подключения/сопоставления |
| 429 | rate_limited; Retry-After и исходный idempotency key |
| 503 | source_unavailable, policy_unavailable; безопасный backoff, мутации не считать завершёнными |
| 500 | internal_error; показать requestId, перед повтором мутации выяснить receipt |

Перечень допустимых codes зафиксирован в schemas; будущие расширения требуют
добавить клиентское неизвестное состояние. Политика и ошибки методов не
вычисляются из скрытых/видимых кнопок UI.

## 8. Проверка и трассировка

Проверки выполнены локально без CRM-вызовов и без изменения canonical API:

```sh
DISTRIBUTION_VALIDATION_PACKAGE_JSON=/Users/nikpeskov/Projects/team-os/package.json \
  node docs/specs/lead-distribution-v1/contracts/fixtures/lead-distribution/verify-contracts.mjs
buf build docs/specs/lead-distribution-v1/contracts/distribution.proto \
  -o /tmp/rs02-distribution.binpb
git diff --check
```

Первый check использует уже установленные `@redocly/ajv` 8.x указанного
проекта и Python с PyYAML; ничего не устанавливает. Проверяет 31 fixtures,
99 JSON schemas, обязательность idempotency/CAS ошибок и 4 enum mapping.
Proto build проверяет синтаксис, номера и типизированные references.
Это **не** runtime contract tests двух backend и не доказательство RBAC,
leases или фактической CRM-операции.

При реализации добавить интеграционные сценарии: повтор POST после потерянного
ACK; same key/different payload 409; delayed result старой binding revision;
read/command чужой company/actor; pause после admission; stale availability;
cancel после possible PATCH; независимый fanout; snapshot target без settlement;
параллельная CAS-конфигурация из двух UI. Fixtures этого пакета служат общей
основой, реальные ответы/транзакции проверяются в owner репозиториях.

| Задача | Подготовленный результат |
| --- | --- |
| [РС-02.2.1](https://app.clickup.com/t/869fapt1e) | TeamOS OpenAPI, CompanyDistributionService proto, §2–4, ошибки/пагинация/CAS и план canonical generation |
| [РС-02.2.2](https://app.clickup.com/t/869fapt1k) | Core OpenAPI, delivery/assignment RPC, §5–7, envelope/result recovery и schema/semantic fixtures |
| [РС-02.2](https://app.clickup.com/t/869fapt1b) | Единый proposed пакет для реализации; текущая функциональность не объявлена готовой |
