# РС-02.1. Модель данных, миграции и жизненный цикл распределения

Дата: 2026-10-01. Проектируемый контракт v1 для задач
[РС-02.1.1](https://app.clickup.com/t/869fapt0q) и
[РС-02.1.2](https://app.clickup.com/t/869fapt11).
Этот документ не добавляет runtime-таблицы и не объявляет миграции выполненными.
Модель конкретизирует [правила](01-business-rules.md),
[границы владельцев](02-architecture-and-access.md) и
[пилот](03-pilot-and-acceptance.md). Названия будущих таблиц и технические
параметры — проектные решения; действующее поведение перечислено отдельно.

## 1. Проверенная база и совместимость

| Источник текущего кода | Наблюдение и следствие |
| --- | --- |
| [TeamOS: distribution migration](../../../../team-os-backend/services/company/migrations/000002_phase6_schedule_distribution.up.sql), [queries](../../../../team-os-backend/services/company/internal/storage/queries/distribution.sql) | `distribution_groups` содержит UUID-массивы участников, active, три algorithm; `distribution_events` использует локальный deal_number. Не переопределять их как CRM-эпизоды |
| [TeamOS: provisioning](../../../../team-os-backend/services/company/migrations/000009_provisioning.up.sql) | Есть company_integrations, user_external_identities и `(company_id,id)` у users. Старый provider/account сохраняется; новый binding не создаёт компанию заново |
| [TeamOS: исходный outbox](../../../../team-os-backend/services/company/migrations/000001_init.up.sql) | Outbox имеет subject/published_at для внутренних событий. HTTP-доставка в Core требует собственного подтверждения; публикация в NATS не подтверждает Core |
| [Core: schema](../../../migrations/000001_init.up.sql), [workflow/effects](../../../migrations/000003_webhook_workflow_correlation.up.sql) | Есть installations, inbox_events, jobs, idempotency_keys, effects; у каждого свой жизненный цикл и FK. CRM assignment требует собственного durable receipt, а не переиспользования смысла lead-status workflow |
| [Core: Activity delivery](../../../migrations/000011_activity_delivery.up.sql) | Пример атомарного receipt + outbox и lease. Distribution не добавляется как разрешённый target Activity |
| [Core: cleanup](../../../internal/maintenance/cleanup.go), [retention migration](../../../migrations/000014_core_redelivery_horizon.up.sql) | Есть очистка tombstones, effects, jobs и календарный минимум redelivery 7 дней. Нельзя считать существующие записи вечной дедупликацией distribution |
| [Core: jobs](../../../internal/jobs/store.go) | Claim/heartbeat/result ограничены worker/attempt/lease. Истечение lease не означает, что CRM не получила PATCH |
| [Core: parser](../../../internal/webhook/parser.go), [router](../../../internal/webhook/store.go) | Хеш события зависит от payload/времени; route один на семейство. Нужен consumer fanout, а не второй register того же ключа |

Новые поля TeamOS остаются аддитивными и camelCase в REST. Старые статусы
симуляции, алгоритмы и reset истории не меняют значения и не очищают реальную
очередь. Текущий последний номер миграции TeamOS `company` — 000021, Core —
000017; будущие номера выбираются при реализации после повторной проверки ветки.
Номера 000022/000018 здесь не резервируются и готовых миграций не подразумевают.

## 2. Общие типы, ключи и изоляция

Во всех таблицах ниже поля обязательны (`NOT NULL`), если явно не указано `?`.

| Обозначение | PostgreSQL / правила |
| --- | --- |
| UUID | `uuid`; локальные ID генерируются один раз до retry |
| CRM ID | `bigint CHECK (>0)`; REST decimal-string без ведущих нулей, максимум 9223372036854775807; не JS number |
| Revision | `bigint CHECK (BETWEEN 1 AND 9007199254740991)`; JSON integer безопасного диапазона, protobuf uint64 с той же проверкой; переполнение отклоняется |
| Digest | `bytea CHECK (octet_length(...)=32)`; SHA-256 канонического нормализованного запроса, не произвольного JSON-порядка |
| Timestamp | `timestamptz`; хранить UTC; даты графика `date`, пояс — валидное IANA-имя `text` |
| State/reason | `text` с CHECK допустимого enum; reason code стабилен, русский текст формируется отдельно; техническая ошибка санитизируется |
| JSON | `jsonb CHECK (jsonb_typeof(...)='object')`; версия схемы, только минимальные данные решения, без OAuth/JWT/raw production payload |

Для каждой TeamOS таблицы с surrogate `id UUID PRIMARY KEY` имеются `company_id`
и `UNIQUE(company_id,id)`. Все локальные ссылки на tenant-сущность составные:
`FOREIGN KEY(company_id,entity_id) → entity(company_id,id) ON DELETE RESTRICT`.
`company_id → companies(id) RESTRICT` защищает сохраняемую работу от удаления
компании по каскаду. Для immutable snapshots ID уже удалённого пользователя —
историческое значение без FK; текущий mapping имеет FK и отдельный lifecycle.
FK на БД другого сервиса/на CRM запрещены. Не ставить `CASCADE` на журнал/receipt.
Для денормализованных binding scope полей у version-таблицы нужен дополнительный
UNIQUE `(company_id,binding_id,revision,account_id,installation_id,integration_id)`
(в Core — тот же tuple без локального company FK); дочерний составной FK ссылается
на весь tuple. Это исключает несовпадающий account при формально верном binding ID.

В Core установка определяется сервером из подтверждённой связи. Создаётся
составной ключ installations `(id,integration_id,account_id)` для FK новых таблиц.
В каждом запросе проверяются company/binding/installation/integration/account;
один UUID или `accountId` браузера прав не даёт. Application predicates обязательны
даже при FK. Эта схема не утверждает, что RLS уже реализован.

Глобальные uniqueness account/lead защищают от второго writer при смене компании,
установки или revision. При конфликте другого tenant API возвращает безопасную
ошибку без имени/ID чужой компании; подробности доступны только оператору.

## 3. TeamOS company: связи и правила

### 3.1. `distribution_bindings` и версии

| Поля | Тип / смысл |
| --- | --- |
| id, company_id | UUID, общие PK/FK |
| revision | Revision текущего желаемого состояния |
| account_id, integration_id, installation_id | CRM ID, UUID, UUID; внешние ссылки |
| state | `pending / active / paused / revoking / revoked` |
| acknowledged_revision? | Revision, `<=revision`; подтверждение Core |
| verified_by_user_id, verified_crm_user_id | UUID исторического actor, CRM ID; проверка прав выполнена сервером |
| created_at, updated_at, activated_at?, revoked_at? | Timestamp |

`UNIQUE(account_id) WHERE state IN ('pending','active','paused','revoking')`;
`UNIQUE(company_id) WHERE` те же состояния — пилот допускает одно подключение.
Индекс `(company_id,state)` обслуживает bootstrap. Смена account/installation —
новая версия связи, а не переприсвоение scope уже принятой операции.

`distribution_binding_versions`: PK `(company_id,binding_id,revision)`, FK binding;
полный immutable tuple account/integration/installation, desired_state, actor и
created_at. Binding ссылается на свою текущую версию отложенным составным FK;
вставка версии и переключение current revision атомарны. Операции/эпизоды
ссылаются на конкретную версию, поэтому история не зависит от current row.
Binding становится active только при подтверждении этой revision обеими сторонами.
Revoking не освобождает account до сверки всех внешних операций.

`distribution_binding_intents`: id UUID PK, company_id FK, account_id,
nonce_hash Digest UNIQUE, requested_by UUID, expires_at, consumed_at?,
confirmed_installation_id? UUID, created_at. Одноразовое потребление — atomic
UPDATE с `consumed_at IS NULL AND expires_at>now()`; секрет nonce в БД не хранится.
Срок намерения задаёт контракт подключения; просроченная запись не активирует binding.

### 3.2. `distribution_employee_mappings`

Поля: id UUID PK, company_id, account_id CRM ID, user_id? UUID,
user_id_snapshot UUID, crm_user_id CRM ID,
state `verified / unavailable / ambiguous`, mapping_revision Revision,
verified_at Timestamp, crm_active boolean, created_at/updated_at Timestamp.
FK `(company_id,user_id)→users(company_id,id) ON DELETE SET NULL (user_id)`;
UNIQUE `(account_id,crm_user_id)` и `(company_id,account_id,user_id)`.
CHECK `state<>'verified' OR user_id IS NOT NULL`; snapshot сохраняет исходный UUID.
Проверка account текущей связи выполняется транзакционно, revision сохраняется
в решении. Mapping не выдаёт прав и не импортирует роли.

Исторические версии mapping сохраняются в `distribution_mapping_versions`
с PK `(company_id,mapping_id,revision)`, полями user_id/account_id/crm_user_id/state/
verified_at/created_at и составным FK mapping. При удалении сотрудника текущий
mapping переводится в unavailable до SET NULL, новая mapping revision и invalidation
сохраняются в той же транзакции; исторические снимки не удаляются. Это обеспечивает
application lifecycle с DB-trigger защитой старого writer во время rollout.
FK не снимается, а действующий endpoint удаления не удаляет историю каскадно.

### 3.3. `distribution_rules` и `distribution_rule_revisions`

| Поля правила | Тип / ограничение |
| --- | --- |
| id, company_id, group_id, binding_id | UUID; FK group, binding в том же company |
| account_id, pipeline_id, status_id | CRM ID; tuple совпадает с binding version |
| revision, binding_revision | Revision |
| state | `draft / active / paused / archived` |
| timezone | IANA text; нормализуется/проверяется приложением |
| algorithm | text CHECK `round_robin` для нового runtime v1 |
| keep_current_available_responsible | boolean DEFAULT true |
| created_at, updated_at | Timestamp |

Текущая группа получает дополнительный `UNIQUE(company_id,id)` для составного FK.
UNIQUE `(company_id,group_id)` для одного правила группы в v1. Точку входа
резервирует UNIQUE `(account_id,pipeline_id,status_id) WHERE state IN ('active','paused')`:
пауза не позволяет второй группе захватить ожидающие сделки; для передачи требуется
контролируемый вывод старого правила. Draft/archived не резервируют точку.
Индекс `(company_id,binding_id,state)`; обновление `WHERE revision=:expected`
меняет правило и вставляет revision в одной транзакции, ноль строк → conflict.

`distribution_rule_revisions`: PK `(company_id,rule_id,revision)`; FK rule;
binding_id/binding_revision, account_id/pipeline_id/status_id, state, timezone,
algorithm, keep flag, members_snapshot JSON, changed_by UUID, created_at.
`members_snapshot` содержит упорядоченные TeamOS UUID и disabled-признаки;
проверка уникальности UUID/принадлежности компании обязательна при записи.
FK `(company_id,binding_id,binding_revision)` на immutable binding version.
Current rule имеет отложенный FK на текущую revision. История append-only.

Группа остаётся владельцем name/description/member_ids/disabled_member_ids,
revision правила фиксирует её точный снимок для расчёта. Новый versioned PUT rule
атомарно обновляет group+rule+snapshot по expectedRevision. Для связанной управляемой
группы старый PATCH/DELETE получает `409 distribution_rule_managed`; UI использует
новый контракт, старые unbound-группы работают как раньше. Эта защита включается
с момента создания управляемого rule, не только на active, чтобы paused/draft
не получали изменения в обход CAS. Activation блокируется до завершения rollout
всех writers; DB-guard не позволяет старому процессу молча перезаписать managed group.
Unsupported algorithm нового runtime отклоняется, legacy значения не удаляются.
Удаление связанной группы — архивирование/отказ при живой работе, не каскад в истории.

### 3.4. `distribution_company_state` и `distribution_group_cursors`

`distribution_company_state`: company_id UUID PK/FK, availability_revision Revision,
updated_at Timestamp. Ревизия увеличивается при любой правке графика, исключения,
сотрудника, mapping или timezone; изменения и invalidation/outbox атомарны.
При rolling update гарантировать охват старых writers транзакционными адаптерами
или временными DB-trigger invalidation; один новый HTTP endpoint недостаточен.

`distribution_group_cursors`: company_id/group_id UUID составной PK и FK group,
cursor_version Revision, last_confirmed_episode_id? UUID FK episode,
last_selected_user_id? UUID snapshot, next_scan_user_id? UUID snapshot,
rule_revision Revision, updated_at. next_scan задаёт включительную границу
обхода, а не «пропустить этого участника». `[A,B,C], last=A, remove A → next=B`.
Смена состава меняет boundary без фиктивного назначения; подтверждение результата
и продвижение cursor транзакционны и защищены от дубля финализации episode.

## 4. TeamOS company: эпизоды, решения и эксклюзивная обработка

### 4.1. `distribution_episodes`

| Поля | Тип / смысл |
| --- | --- |
| id, company_id, binding_id, rule_id, group_id | UUID, локальные составные FK |
| binding_revision, rule_revision | Revision, FK immutable versions |
| account_id, lead_id, pipeline_id, status_id | CRM ID, immutable scope эпизода |
| episode_sequence | bigint >0; локальный порядковый номер для account+lead |
| entry_source_event_id?, entry_fingerprint? | UUID источника, Digest; внешний ID без FK на Core |
| entry_evidence | `observed_transition / created_in_stage / reconciled / manual_backfill / ambiguous` |
| entry_occurred_at?, received_at, created_at, updated_at | Timestamp; occurred_at не задаёт порядок worker |
| row_version | Revision для локального CAS |
| state | enum таблицы §7 |
| reason_code?, reason_details? | text / JSON безопасных причин |
| observed_responsible_id?, planned_responsible_id? | CRM ID; observed snapshot и предварительный получатель |
| planned_user_id? | UUID snapshot TeamOS, не вечный FK к сотруднику |
| decision_id?, operation_id? | UUID; decision — составной локальный FK, operation — внешний ID |
| next_attempt_at?, last_checked_at?, exited_at?, finished_at? | Timestamp |
| cancellation_requested_at?, cancellation_reason? | Timestamp/text; запрос не равен отмене |

UNIQUE `(account_id,lead_id,episode_sequence)`; sequence выделяется под локом
`distribution_lead_heads` (§4.3), а не MAX+1 без блокировки. Нет UNIQUE только на
lead+stage: новый вход обязан помещаться рядом со старым эпизодом.
UNIQUE `(company_id,rule_id,entry_fingerprint)` WHERE fingerprint IS NOT NULL
допустим только для доказанно отдельного entry evidence, а не хеша произвольного
веб-хука. Неоднозначность не создаёт автоматическую команду.

Индексы: `(company_id,group_id,state,received_at,id)` для UI;
`(next_attempt_at,received_at,id) WHERE state IN ('checking','waiting_shift','needs_configuration')`;
`(account_id,lead_id,received_at DESC,id)` для сверки; `(operation_id)` WHERE NOT NULL;
`(company_id,rule_id) WHERE finished_at IS NULL` для invalidation.

`distribution_episode_sources`: PK `(company_id,source_system,source_event_id)`;
episode_id UUID FK, source_fingerprint Digest, received_at. Несколько add/status
одного входа могут ссылаться на один episode. Запись source receipt не доказывает
новый вход; при неизвестной границе хранится evidence ambiguous и needs_configuration.
Новый valid reentry получает новую sequence, даже если timestamp совпадает.

### 4.2. `distribution_decisions` и `distribution_history`

Decisions: id/company_id/episode_id/rule_id/binding_id UUID; decision_sequence bigint >0;
rule_revision, binding_revision, availability_revision, cursor_version Revision;
decision_kind `assign / keep / wait / reject / cancel`; source_snapshot JSON
(account/lead/pipeline/status/responsible/observedAt/updatedAt?); availability_snapshot
JSON (candidate IDs, причины, интервалы и mapping revisions); target_user_id? UUID,
target_crm_user_id? CRM ID, reason_code text, next_attempt_at? Timestamp,
created_at Timestamp. UNIQUE `(company_id,episode_id,decision_sequence)`;
индекс `(company_id,episode_id,created_at,id)`; FK episode и immutable rule version.
После записи не изменять: повторный расчёт — новое решение.

History: id/company_id/episode_id UUID, sequence bigint >0, kind text,
decision_id? UUID, operation_id? UUID, operation_result_version? Revision,
previous_state? text, next_state text, reason_code text, actor_type text,
actor_id? text, details JSON, created_at. UNIQUE `(company_id,episode_id,sequence)`;
UNIQUE `(company_id,operation_id,operation_result_version)` WHERE both NOT NULL;
индекс `(company_id,episode_id,created_at,id)`. Содержимое неизменно.
ResultVersion одного Core operation не может дважды продвинуть cursor.

`distribution_operation_mirrors` (TeamOS): operation_id UUID PK, company_id UUID,
episode_id/decision_id UUID с локальными составными FK, binding_id/binding_revision,
account_id/lead_id, last_result_version? Revision, status text,
external_effect_state text, outcome? text, confirmed_snapshot? JSON,
last_payload_hash? Digest, received_at?/finished_at? Timestamp, created_at/updated_at.
UNIQUE `(company_id,decision_id)`; индекс `(company_id,episode_id,created_at)`.
Это локальная проекция Core result, не второй владелец внешней операции.
CAS `last_result_version IS NULL OR last_result_version<:incomingVersion`
проводится под lock строки; одинаковая версия сравнивает hash. Mirror сохраняется
для каждой старой operation, поэтому смена текущего decision/operation у episode
не лишает возможности обработать поздний результат прежней попытки.

### 4.3. Head против guard: новый эпизод разрешён, второй writer запрещён

`distribution_lead_heads`: PK `(account_id,lead_id)`, company_id UUID,
last_sequence bigint >=0, current_observed_episode_id? UUID,
last_pipeline_id?/last_status_id?/last_responsible_id? CRM ID, observed_at? Timestamp,
evidence_version Revision, updated_at. `UNIQUE(company_id,account_id,lead_id)` для
tenant-FK; head хранит последовательность наблюдений, а не гарантию истории CRM.

`distribution_lead_claims`: PK `(account_id,lead_id)`, company_id UUID,
group_id/episode_id/decision_id UUID, operation_id UUID UNIQUE,
claim_version Revision, claimed_at Timestamp, released_at? Timestamp,
release_evidence? JSON. FK head и локальные сущности включают company_id.
Живой claim — `released_at IS NULL`; уникальность одного живого group claim
обеспечивается partial UNIQUE `(company_id,group_id) WHERE released_at IS NULL`.
Строка account+lead переиспользуется только после доказанного release, история
старого claim записывается в history. При takeover executor меняется lease,
но claim бизнес-операции не исчезает.

Новый episode, другая группа, bindingRevision или installation не создают новый
независимый guard: они ждут `waiting_previous_operation`. Guard дублируется в Core
по **account+lead без installation в PK** как последняя защита внешнего writer.
Guard не протухает по времени. Закрытие UI, pause и cancel не освобождают его.
Групповой слот держится при unknown; после доказанной ошибки до отправки освобождается.

Для предотвращения взаимных блокировок порядок locks фиксируется: company/binding,
lead head, group cursor (несколько групп по UUID), claim/episode. Сетевой вызов
никогда не выполняется внутри долгой SQL-транзакции.

## 5. Core: binding, операция, попытки и receipts

### 5.1. Проверенный scope и принятие команды

`distribution_binding_grants`: binding_id UUID PK, company_id UUID (внешний),
binding_revision Revision, installation_id/integration_id UUID, account_id CRM ID,
state `pending / active / paused / revoking / revoked`, acknowledged_at? Timestamp,
updated_at Timestamp. FK `(installation_id,integration_id,account_id)` на installations;
UNIQUE account_id при reserving states как в TeamOS. История в
`distribution_binding_grant_versions` с PK `(binding_id,binding_revision)`,
полным immutable scope, желаемым state и created_at. Команда связывается с версией;
текущий live grant/capability проверяется отдельно при admission и перед эффектом.

`distribution_operation_receipts`: operation_id UUID PK, binding_id UUID,
binding_revision Revision, company_id UUID, installation_id/integration_id UUID,
account_id CRM ID, action text CHECK `assign_responsible`, key_hash Digest,
request_hash Digest, first_received_at Timestamp, response_status integer,
response_body JSON. UNIQUE `(binding_id,action,key_hash)` не включает revision:
переиспользование ключа с иной revision — payload conflict, не новая операция.
Одинаковый ключ+request hash возвращает тот же operation ID и исходную квитанцию;
один operationId с другим hash отклоняется независимо от Idempotency-Key.
Canonical hash включает весь scope, episode/decision, ожидаемое состояние и target;
traceId/transport retry timestamp в него не входят.

Admission транзакция: проверить scope → вставить receipt → вставить operation +
обычный Core job + operation event/outbox → commit → ответ. Durable ACK не значит
назначение. Повтор запроса после потерянного ACK не создаёт второй job.

### 5.2. `distribution_operations`

| Поля | Тип / ограничение |
| --- | --- |
| id | UUID PK/FK receipt.operation_id RESTRICT |
| binding_id/binding_revision, company_id, installation_id/integration_id/account_id | Scope принятой команды; составные FK grant-version и installation tuple |
| episode_id, decision_id, rule_id, group_id | UUID внешних business IDs, без cross-DB FK |
| decision_kind | `assign / keep`; immutable часть canonical request hash |
| rule_revision, availability_revision | Revision; снимок разрешения, не вечная авторизация |
| lead_id, expected_pipeline_id, expected_status_id | CRM ID |
| expected_responsible_id, target_responsible_id | CRM ID; если CRM не даёт валидный current owner, precheck fail-closed |
| expected_updated_at?, source_observed_at | Timestamp; updated_at не CAS-token CRM |
| authorization_expires_at | Timestamp; после истечения новое подтверждение решения без автоматического нового PATCH |
| status | enum §7, независим от job.status |
| outcome? | `assigned / kept / already_target / source_changed / cancelled / rejected / conflict`; конкретизирует результат и не заменяет status |
| external_effect_state | `no_attempt / in_flight / unknown / settled` |
| result_version, row_version | Revision |
| job_id? | UUID; FK `(job_id,installation_id)`→jobs(id,installation_id) RESTRICT пока pinned |
| first_attempt_at?, cancel_requested_at?, finished_at? | Timestamp |
| last_observation?, resolution_evidence? | JSON; наблюдаемое состояние, время/метод, основание окончательного результата |
| error_code?, created_at, updated_at | text/Timestamp |

UNIQUE `(company_id,decision_id)` запрещает два operation ID одному решению.
Индексы `(account_id,lead_id,created_at DESC)`, `(installation_id,status,created_at)`,
`(status,updated_at) WHERE status IN ('applying','outcome_unknown','confirming')`.
`finished_at` допустим только для терминального состояния при settled/no_attempt.
Результат no_change требует precheck target=current без отправленного PATCH.

`distribution_operation_attempts`: PK `(operation_id,attempt_no)`; attempt_no int>0,
job_attempt int>0, executor_id text, lease_token UUID, fence bigint>0,
request_started_at?, request_finished_at?, http_status? int, transport_outcome
`not_sent / response / ambiguous`, error_code? text, observed_state? JSON,
created_at/updated_at Timestamp. Сохранять intent **до** отправки. Один неизвестный
attempt запрещает второй PATCH; GET-сверки оформляются отдельными observation
записями, не увеличивая счётчик внешних записей.

`distribution_operation_observations`: id UUID PK, operation_id UUID FK,
observation_sequence bigint>0 UNIQUE с operation_id, kind `get / webhook / operator`,
snapshot JSON, observed_at Timestamp, source_event_id? UUID,
created_at Timestamp. Webhook семантически коррелируется, но не доказывает авторство.

`distribution_operation_results`: PK `(operation_id,result_version)`, scope
binding/company/account/installation, status, external_effect_state,
outcome? text, confirmed_snapshot? JSON, reason_code?, evidence JSON, emitted_at Timestamp.
Append-only; запись и outbox атомарны. Поздний result меньшей версии не регрессирует
TeamOS. Одинаковая версия с другим hash — конфликт протокола, не молчаливая замена.

### 5.3. Core `distribution_lead_guards`

PK `(account_id,lead_id)`, operation_id UUID UNIQUE FK, binding_id UUID,
company_id UUID, claim_version Revision, holder_job_id? UUID,
fence bigint>0, executor_lease_token? UUID, executor_lease_until? Timestamp,
claimed_at Timestamp, released_at? Timestamp, release_evidence? JSON.
Claim выполняется CAS/lock и проверяет существующую operation во **всех** установках.
Executor lease — механизм локального worker, отдельный от guard внешнего эффекта.
Попытка освободить guard только по истекшему lease запрещена.

Реиспользовать Core jobs fencing как авторитетный номер попытки; поля guard отражают
тот же attempt/token и проверяются атомарно с переходом операции, а не создают
второй независимый scheduler. При несовпадении fence результат устаревшего worker
не принимается; его свидетельство о внешнем запросе сохраняется отдельно для
сверки. Fencing не может отменить уже уходящий сетевой запрос в amoCRM.

## 6. Inbox/outbox и независимая доставка каждому потребителю

В **каждом владельце отдельно** создаются `distribution_consumer_inbox` и
`distribution_bridge_outbox`; общий SQL между ними отсутствует. В Core добавить
`webhook_consumer_deliveries` для fanout к lead-status и lead-distribution.

| Таблица | Поля, ограничения и индексы |
| --- | --- |
| consumer_inbox | consumer_id text, source_system text, message_id UUID — составной PK; scope company/binding/account + binding_revision; message_kind/schema_version, payload_hash Digest, payload? JSON; state `received/processing/applied/rejected`; attempts int>=0, next_attempt_at, lease_token?/lease_until?, outcome? JSON, received_at/applied_at?; UNIQUE scope не заменяет PK, scope/hash проверяются при дубле; ready index `(consumer_id,next_attempt_at,message_id) WHERE state IN ('received','processing')` |
| bridge_outbox | id UUID PK, source_message_id UUID, consumer_id text, scope tuple, message_kind/schema_version, payload JSON, payload_hash Digest, state `pending/delivering/acknowledged/blocked`, attempts int>=0, next_attempt_at, lease_token?/lease_until?, acknowledged_at?, error_code?, created_at/updated_at; UNIQUE `(consumer_id,source_message_id)`; ready index `(next_attempt_at,id) WHERE state IN ('pending','delivering')`; lease-expiry index для delivering |
| webhook_consumer_deliveries (Core) | installation_id UUID, event_fingerprint Digest, consumer_id text — PK; source_inbox_event_id? UUID composite FK inbox_events ON DELETE SET NULL для ID; distribution_event_id UUID UNIQUE, normalized_snapshot JSON, state `pending/processing/delivered/ignored/blocked`, attempts int>=0, next_attempt_at, lease_token?/lease_until?, created_at/finished_at?; ready index `(consumer_id,next_attempt_at,distribution_event_id)` |

Составные scope-FK добавляются к grant/binding version в соответствующей БД.
Lease-поля парные; ACK update требует совпадения lease_token. `processing` после
истечения lease возвращается на обработку; `applied` никогда не исполняется вновь.
Consumer ID включает версию бизнес-контракта (`teamos-distribution-v1`,
`core-distribution-commands-v1`, `core-lead-status-v1`), не имя worker/экземпляра.
Рестарт replica не создаёт нового consumer. Новая версия consumer не разрешает
повторить CRM-эффект: operation/episode receipts продолжают защищать бизнес-identity.

Fanout транзакция сохраняет нормализованный snapshot для каждого включённого
потребителя; сбой distribution не откатывает уже завершённый lead-status consumer.
Существующий single router должен стать dispatcher с durable children, а не
последовательным вызовом двух функций без журналирования результата каждой.
Consumer subscription фиксируется на событии; исторические события автоматически
не прогоняются новому consumer при включении модуля.

Потребитель подтверждает HTTP после commit inbox. Применение в TeamOS отдельной
транзакцией: lock inbox + head → открыть/обновить episode + history + при нужде
decision/claim/command-outbox → отметить inbox applied. Core принимает команду
receipt-транзакцией §5.1. Core завершает операцию транзакцией result+outbox;
TeamOS применяет result с CAS result_version и атомарной финализацией cursor.
Потеря ACK порождает повтор доставки, не второй бизнес-эффект.

Поздний result старой bindingRevision нельзя просто отбросить после rebind/revoke:
его проверяют по сохранённому immutable scope **этой** operation и идентичности
отправителя. Он может завершить старый episode, дополнить audit и освободить
соответствующий guard при достаточном evidence. Он не меняет текущий grant,
не начинает новую decision и не подтверждает operation нового binding.
Меньшая resultVersion игнорируется только относительно уже принятой версии той
же operation, а не относительно current bindingRevision. Перед освобождением
проверяется `guard.operation_id=:oldOperationId`, чтобы не освободить новый claim.

## 7. Состояния и доказательства переходов

### 7.1. Бизнес-очередь TeamOS

| Wire state | Значение / переход |
| --- | --- |
| checking | Новый/пересчитанный episode; свежие scope/CRM/графики → любой подходящий следующий шаг |
| waiting_shift | Есть будущая смена; наступление next_attempt_at или invalidation → checking |
| needs_configuration | Нет валидных данных или неоднозначен entry; исправление/проверка → checking |
| paused | Группа/связь на паузе; resume → checking, после новой проверки |
| waiting_previous_operation | Старый внешний исход этой сделки ещё неизвестен; подтверждённое разрешение guard → checking |
| assigning | Отправлена/принята одна operation; результат → assigned/unchanged/error/cancelled либо outcome_unknown |
| outcome_unknown | PATCH мог дойти, результата недостаточно; GET/сверка → assigning или финализация при достаточных evidence; возраст не переводит в cancelled |
| assigned | Требуемый target подтверждён, ход RR учтён ровно раз |
| kept | keep=true, текущий доступный участник сохранён; RR без изменения |
| unchanged | keep=false, выбран текущий target, no_change подтверждён; RR +1, фактических смен +0 |
| cancelled | Доказанная отмена до отправки или после доказанного no-effect; выход после уже применённого эффекта фиксируется отдельно, не переписывает его |
| error | Исправимая конфигурационная/постоянная ошибка или конфликт; новый расчёт допускается только после разрешения внешнего исхода старой operation |

`assigned/kept/unchanged/cancelled` финальны для решения; reentry — новый episode.
Для `error` явный retry создаёт новую decision, но не стирает предыдущую operation.
Finished episode не становится checking от обычного update владельца.

### 7.2. Техническая operation Core

| Переход | Условие и запись |
| --- | --- |
| admission → queued | Receipt, job и операция committed; external_effect_state=no_attempt |
| queued → prechecking | Lease и account+lead guard получены, проверяются live grants и бизнес-разрешение |
| prechecking → no_change | target уже текущий до первой попытки; записать snapshot, settled, result/outbox |
| prechecking → rejected/cancelled/conflict | Нет попытки отправки: отказ прав/подтверждённая отмена/исходный state не совпал; result/outbox и release guard |
| prechecking → applying | Зафиксированы intent/attempt/fence; затем внешний PATCH; effect=in_flight |
| applying → confirming | Получен определённый ответ, нужно контрольное наблюдение CRM |
| applying/confirming → outcome_unknown | Transport ambiguity, crash, stale fence или неясный внешний эффект; effect=unknown, guard остаётся |
| confirming → succeeded | Требуемое состояние подтверждено, основания завершения зафиксированы, effect=settled; terminal result/outbox |
| confirming → conflict/rejected | Имеется определённый завершённый запрос и подтверждённое несовпадение/отказ; release только при settled |
| outcome_unknown → confirming | Получены дополнительные свидетельства; сначала сверка без второго PATCH |
| any nonterminal + cancel request | Записать cancel_requested_at; операция до отправки может стать cancelled, после отправки должна выяснить исход |

Сетевой retry до доказанной отправки может переиспользовать ту же operation.
После возможной отправки повтор PATCH запрещён, пока не выяснен исход; «GET
показал прежнего владельца» само по себе не доказывает, что поздний запрос не придёт.
Job `dead` по max_attempts не переводит operation unknown в терминальную ошибку.
Техническая исчерпанность попыток переводит работу на ручную сверку, guard сохраняется.

`succeeded` подтверждает наблюдаемое достижение состояния, не авторство. Если
совпадение пришло только через GET/webhook при всё ещё открытом неопределённом
запросе, сохранить наблюдение и оставить confirming/unknown: late PATCH не
отменяется наблюдением. Неопределённость снимается по достаточному свидетельству
завершённой внешней попытки либо явной документированной операторской процедуре
с признанием остаточного риска; такая процедура не именуется гарантированным CAS.
В пилоте нет автоматического «освободить через N минут» и нет второй записи
поверх unresolved. Человек получает причину и evidence, а не ложный успех.

`no_change + already_target` означает выбранного по RR текущего владельца,
а `no_change + kept` — подтверждение сохранения при keep=true. Входящий decision
kind обязателен; Core не выбирает эту бизнес-политику самостоятельно. TeamOS
применяет правила продвижения хода по decision+outcome, не только по HTTP/status.

Между live precheck и PATCH остаётся окно ручного изменения. Revision TeamOS,
lease и GET/compare защищают локальное решение, но не блокируют amoCRM. Контрольный
GET и история выявляют наблюдаемый конфликт; абсолютное предотвращение внешних
гонок или exactly-once доставка/эффект не являются гарантиями этого контракта.

## 8. Retention, tombstones и восстановление

Предлагаемая политика v1 отделяет bulky payload от компактной identity:

| Данные | Хранение / разрешение очистки |
| --- | --- |
| Незавершённые episode/decision/operation, unknown/confirming, guards, неACK outbox | Не удалять по возрасту; держать до доказанного завершения либо явного операторского разрешения с audit |
| Compact operation receipt/key_hash/request_hash/final identity | В v1 бессрочно; старый ключ никогда не начинает новую operation после очистки UI-истории |
| Consumer inbox tombstone (consumer/source/message/hash/outcome/scope) | В v1 бессрочно; payload после финализации можно убрать, tombstone остаётся |
| Episode source identity и sequence/head | В v1 бессрочно; cleanup не превращает старый вход в новый |
| ACK outbox payload, подробные observations/история завершённых | Проектный floor 90 дней после finish/ACK и отсутствия unresolved ссылок; фактическое включение cleanup отдельной задачей с проверками |
| Raw webhook/inbox Core | Текущая policy допустима лишь после durable копии per-consumer и отсутствия активных ссылок; distribution identity переживает Core raw/tombstone cleanup |
| Графики/имена сотрудников в snapshot | Минимум для объяснения решения; не копировать PII без необходимости; удаления требуют сохранить безопасную ID-identity и объяснимость незавершённой работы |

90 дней — проектное значение для payload, не срок безопасного забывания эффекта.
Pending outbox не «истекает» автоматически через текущие Core 7 дней. После
операционного предела доставка становится blocked с причиной и ручным retry
той же message identity. Retry горизонт не длиннее хранения receiver tombstone;
для v1 identity сохранена бессрочно. Изменение этого правила требует отдельной
процедуры архивирования и запрета replay за границей доказуемости.

Реализация должна добавить anti-join pinning в существующий cleanup jobs/effects/
inbox, или хранить независимую достаточную копию и отпускать FK только после
доказанного завершения. Одного FK RESTRICT недостаточно: cleanup не должен
падать всем batch из-за защищённой строки. Для distribution использовать собственные
attempt/observation таблицы вместо необдуманного продления lead-status effects.

После coordinated restore обеих БД сначала приостановить mutating workers,
сверить receipt/result/outbox по stable operationId и определить missing ACK.
Рассогласованные snapshots не дают права повторить PATCH. Состояние источника CRM
проверяется; old unknown и guards восстанавливаются до приёма новых команд.

## 9. Порядок аддитивных миграций

1. **Expand TeamOS:** новые таблицы/составные unique/FK, новые nullable references,
   без изменения distribution_events и без заполнения source-текста CRM ID.
   Индексы больших таблиц строить отдельной migration с учётом concurrent DDL;
   FK при необходимости `NOT VALID` → проверка → `VALIDATE CONSTRAINT`.
2. **Expand Core:** receipt/operation/guard/consumer таблицы, composite installation
   key; зарегистрировать новый job/capability отдельным продуктом, выключенным
   по умолчанию. Enum старых jobs/idempotency/Activity не менять под business states.
3. **Deploy совместимых readers:** старые экраны сохраняют старые поля; новые
   эндпоинты/схемы имеют явную версию. Наличие таблиц не включает module.
4. **Deploy всех writers/invalidation:** для managed group включить versioned
   rule PUT и legacy mutation guard; связать employee/schedule mutations с
   availability revisions. До окончания rolling update activation
   запрещена, если старый процесс способен обойти эти проверки.
5. **Backfill только проверенных identity:** сохранить user UUID, membership и
   графики; сформировать proposed mapping по company+account+CRM ID; неоднозначные
   записи требуют решения. Binding pending, rule draft; никаких automatic active.
6. **Включить per-consumer fanout и durable bridge:** сверить existing lead-status
   consumer, receipts и unknown pinning cleanup; отправить тестовые synthetic
   сообщения. Уже обработанные raw-события не replay автоматически.
7. **Подтверждение binding и пилот:** обе стороны ack одной revision, валидация
   уникальности точки/аккаунта, наблюдение без PATCH; затем отдельное управляемое
   включение с отключением старого writer по процедуре пилота.
8. **Contract позже:** старые структуры не удалять в РС-02. Снятие legacy поля/API
   требует отдельной версии и доказательства отсутствия readers.

Rollback приложения сначала приостанавливает новые операции и выясняет in-flight;
данные/receipt/guards сохраняются. Down migration, удаляющая уже использованные
таблицы, запрещена обычным rollback. Удаление возможно только отдельной процедурой
при доказанном отсутствии незавершённой работы, backup и явном разрешении оператора.

## 10. Сценарии проверки модели и готовность РС-02.1

| ID | Проверяемая ситуация | Инвариант модели |
| --- | --- | --- |
| DM-01 | Две параллельные активации одного account/pipeline/status | Partial UNIQUE пропускает одну, другая получает conflict |
| DM-02 | Cross-company group/episode/decision FK | Составной FK отвергает запись; внешний scope дополнительно проверяет API |
| DM-03 | Два add/status одного входа | Inbox/source receipts сходятся в один episode; повтор command key возвращает receipt |
| DM-04 | Exit и reentry с одинаковым timestamp | Новый доказанный вход получает новую sequence; ambiguity сохраняется без auto assignment |
| DM-05 | Rebind installation при старом unknown | Глобальный account+lead guard не даёт новый PATCH |
| DM-06 | Потерян ACK принятия/результата | Stable IDs + payload hash + per-consumer inbox исключают вторую operation/повторный cursor step |
| DM-07 | Один consumer завершён, второй падает | Durable fanout изолирует state каждого, успешный consumer не переисполняется |
| DM-08 | Worker умер после intent или потерял lease | Operation сверяется; old fence не финализирует, guard не исчезает по таймеру |
| DM-09 | Cancel после возможного PATCH | cancel_requested не равен cancelled; unknown сохраняется до evidence |
| DM-10 | keep и same-owner при keep=false | kept не двигает cursor; unchanged двигает один раз без счётчика фактической смены |
| DM-11 | Cleanup через 7/90 дней при unknown | Identity, guard, operation и необходимые evidence/outbox сохраняются |
| DM-12 | Старый group/schedule writer во время rolling deploy | Activation заблокирована до совместимости; managed group требует CAS endpoint, schedule invalidation фиксируется атомарно |
| DM-13 | Ручная смена между GET/PATCH | Не обещать CAS; сохраняются attempt и observation, виден конфликт/unknown |
| DM-14 | Удалён A после подтверждённого выбора из A/B/C | next_scan=B; history и cursor version сохраняются |
| DM-15 | Повтор key с другим payload/revision | Conflict без изменения первого receipt/operation |
| DM-16 | Восстановлен старый backup одного владельца | Mutations приостановлены до сверки двух журналов и CRM; неопределённость не заменена retry |

**РС-02.1.1:** сущности имеют поля/типы, tenant-FK, unique/index, версии binding/rule,
ожидание/entry/ответственных/next_attempt_at, историю и guard; expand/backfill/
rolling/rollback совместимы с существующей схемой. **РС-02.1.2:** определены
operation identity, expected state, results, inbox/outbox по consumer, leases,
unknown/cancel/reentry и хранение без потери identity.

Проверка этого этапа — сверка схемы с миграциями/кодом и сценариями DM-01–16,
согласование wire names/types с документом API, проверка локальных ссылок и Markdown.
SQL в runtime migrations не создан и PostgreSQL DDL здесь не исполнялся;
следующий этап реализации обязан проверить реальные миграции и конкурентные
транзакции в PostgreSQL. Таблица DM — будущие тесты, не отчёт об их прохождении.
