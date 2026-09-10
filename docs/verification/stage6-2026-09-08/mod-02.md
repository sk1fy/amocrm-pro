# MOD-02. Одинаковое поведение Activity/CRM Events при разном размещении

Дата: 10 сентября 2026. Локальная проверка in-process mTLS, не production и не process cluster.

Размещение меняет composition (embedded / gRPC), а не продуктовую логику. Вторая реализация Activity/Events не писалась: тесты гоняют `activity.New` и тот же owner-фикстурный порт через локальный вызов и `servicerpc`.

Лог: [servicerpc-parity.txt](mod-02/servicerpc-parity.txt).

## Существующее покрытие и добавленное

| Источник | Что уже было | Пробел относительно MOD-02 |
| --- | --- | --- |
| `internal/servicerpc/activity_parity_test.go` | Panel, Configure idempotency, Settings, Operation, граница периода, подделка токена, disabled → PermissionDenied | Нет GetEvent/карточки, compact, categories/group_id/unknown-authors/buckets, NotFound vs empty, раздельного Unauthenticated vs PermissionDenied на карточке |
| `internal/servicerpc/crmevents_parity_integration_test.go` | Apply/Status/Operation local↔mTLS, policy/scope, replay после потери ответа (Postgres inbox) | Нужен Postgres; не покрывает Activity Configure и чтения Panel/GetEvent |
| `internal/servicerpc/stage2_read_integration_test.go` | compact/order HTTP+Postgres | Нужен Postgres; не сравнивает embedded/RPC карточку и presentation-фильтры как отдельный stage-6 набор |
| `internal/servicerpc/stage2_compatibility_test.go` | Старый Events без GetEvent → Unavailable; новые query-поля, включая categories/group_id/unknown/buckets, fail-closed | Нет старого Activity без GetEvent; нет Activity без EventPresenter |
| `internal/componentruntime/process_integration_test.go` | embedded→grpc→embedded; ping и lead-status при остановленном Activity; durable 202, пока получатель лежит | GET `/panel` и GET `/events/{id}` при недоступном Activity не проверялись |
| `internal/componentruntime/process_faults_integration_test.go` | fencing коллектора между OS-процессами | Не дублировался |

Добавлено в `internal/servicerpc/stage6_parity_test.go` (синтетические фикстуры, без Docker/Postgres):

- `TestStage6ActivityPresentationLocalAndMTLSGRPCParity`
- `TestStage6DurableConfigureReplayAfterLostRPCAndRestart`
- `TestStage6ActivityUnavailableIsExplicitNotEmptyHistory`
- `TestStage6OldPeerPresentationAndGetEventFailClosed`

Отдельный process-файл не создавался: HTTP GET `/panel` и `/events/{id}` при падении Activity закрыты focused-тестом через `activitybridge` + остановленный mTLS, без копии topology из `process_integration_test.go`.

## Сценарий → embedded / RPC

Смысл сравнивался JSON-декодером с `UseNumber`. Latency не сравнивалась.

| Сценарий | Embedded | mTLS gRPC | Совпадение |
| --- | --- | --- | --- |
| Полная панель (5 синтетических событий, view списка без details) | 5 событий, ReadVersion=3, payloads_omitted=false | То же | да |
| Compact + order=desc | Payloads пустые, detail_state=omitted, первым chat-system | То же | да |
| GET-эквивалент карточки EventCard (view/details/enrichment, linked_talk_contact_id) | Презентация Activity, JSON enrichment `n:2` сохранён | То же | да |
| Owner GetEvent | Исторический конверт без view | То же | да |
| categories=tasks | Одно событие task-completed-7 | То же | да |
| group_id=3 | Только автор 7 | То же | да |
| group_id=99 (пустой отдел) | Пустая история + empty_reason, не ошибка | То же | да |
| include_unknown_authors + user_ids=7 | Авторы 7, 7, 71999, 0; Bob из другого отдела исключён | То же | да |
| buckets=hour | Timeline не теряется на wire | То же | да |
| Owner Query compact | Совпадает с RPC Query | То же | да |
| Неизвестный category / order / buckets / group_id&lt;0 / период &gt;31д | InvalidArgument | InvalidArgument | да |
| EventCard неизвестный id | NotFound, пустой конверт | NotFound | да |
| EventCard с пробелом в id | InvalidArgument | InvalidArgument | да |
| Подделанный токен (Panel и карточка) | Unauthenticated | Unauthenticated | да |
| Disabled policy | PermissionDenied | PermissionDenied | да |
| Panel/GetEvent не пишут inbox Configure/Apply | writes не растут | writes не растут | да |
| Configure: commit, потеря RPC-ответа, replay | — | Тот же operation id, 1 запись inbox | да |
| Configure с другим payload | Conflict | Conflict | да |
| Apply sync replay (фикстурный inbox) | 1 write | тот же operation | да |
| Рестарт RPC-сервера на том же store | — | replay без второй команды | да |
| Получатель остановлен | — | Unavailable, writes не растут | да |

## Недоступность Activity

`process_integration_test` уже держит Core `/ready`, widget ping и lead-status, пока Activity-процесс остановлен. GET `/panel` и GET `/events/{id}` там нет.

Новый тест:

| Поверхность | Activity недоступен | Результат |
| --- | --- | --- |
| Core Ready + Policy.Issue | Activity не зарегистрирован | SERVING / Issue успешен |
| RPC Panel / EventCard | нет Activity / сервер остановлен | `unavailable`, не пустая история |
| Activity Ready | собственный Ready=error | `unavailable` («service is not ready» / зависимость) |
| Core Ready | Activity Ready down | Core остаётся ready |
| HTTP GET `/api/v1/widget/activity/panel` | gRPC Activity остановлен | 503 `{"error":{"code":"unavailable"}}`, без массива `events` |
| HTTP GET `/api/v1/widget/activity/events/{id}` | Activity down, Events owner жив | 200 исторический конверт без `view` (существующий fallback `activitybridge.GetEvent`) |
| HTTP GET `/events/{id}` | Activity down и Events нет | 503 `unavailable`, не пустая история |

Пустая история и NotFound не подменяют unavailable. Карточка через RPC Activity при падении модуля — явное unavailable; публичный URL детали при живом Events owner сохраняет прежний fallback на конверт владельца.

## Совместимость версий

Уже было (stage 2): старый Events без GetEvent; новые query-поля, включая presentation, не игнорируются молча.

Добавлено:

| Кейс | Ожидание | Результат |
| --- | --- | --- |
| Owner ReadVersion=2 при categories | Unavailable на Panel local и RPC | да |
| Activity без EventPresenter | EventCard → Unavailable (`event presenter unavailable`) | да |
| Старый Activity без метода GetEvent, Panel остаётся | Panel OK; EventCard Unavailable, не пустая карточка | да |
| View/details/enrichment на текущем peer | не отбрасываются protobuf/mTLS | да (parity карточки) |

Новых presentation-опций, которые текущий peer мог бы тихо отбросить, сверх уже закрытых stage 2 (categories, include_unknown_authors, buckets, group_id), не найдено.

## Что не запускалось

- Process cluster (`COMPONENT_PROCESS_TEST_ALLOWED`, OS-процессы api/activity/crm-events)
- Docker / `make activity-ci` / `make activity-test` / `make test`
- Postgres owner-наборы (`crmevents_parity_integration_test`, `stage2_read_integration_test`)
- Живой amoCRM, production deploy
- Нагрузочные пороги и сравнение latency embedded vs сеть

Локально выполнено:

```sh
go test -count=1 -timeout 2m ./internal/servicerpc -run 'TestActivityBusinessLocalAndMTLSGRPCParity|TestStage6'
```

5 PASS (существующий activity parity + 4 новых). Без `-race` в этом прогоне.

## BLOCKED-FOR-COORDINATOR

- Process-level GET `/panel` и ping/lead-status на реальных бинарях при остановленном Activity не подтверждались в этой сессии; RPC/HTTP-адаптер закрывает семантику, cluster — нет.
- Owner-durable Apply после потери соединения остаётся на Postgres-тесте `TestCRMEventsOwnPostgresReplayAfterMTLSConnectionLostAfterCommit`; здесь проверен фикстурный inbox и Activity Configure.
- Чекбоксы плана и GitHub Issues не обновлялись (запрещено заданием).
- Итоговый gate этапа 6 (`make activity-ci`) координатор должен прогнать сам.

## Файлы

- `internal/servicerpc/stage6_parity_test.go` — новые тесты
- `docs/verification/stage6-2026-09-08/mod-02.md` — этот отчёт
- `docs/verification/stage6-2026-09-08/mod-02/servicerpc-parity.txt` — лог `go test`
