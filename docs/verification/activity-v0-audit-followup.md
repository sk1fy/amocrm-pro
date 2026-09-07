# Activity v0: исправления после независимого аудита

> Исторический снимок на 2026-09-06, до коммита и серверного пилота. Дальнейшие
> изменения, сообщение пользователя об успешной живой проверке и результаты
> второго аудита — в [отчёте 2026-09-07](activity-v0-hardening.md).

Дата: 2026-09-06. Основание — аудит пользователя по редакции v2. Изменения
остаются в working tree относительно `a33a845f2db23b46c96fc7199d1161f654ad86f5`.
Это дополнение к [исходному отчёту](activity-v0-results.md), а не переатрибуция
его логов и manifest новым исправлениям. Пилот в amoCRM по-прежнему не принят.

## Исправлено

1. **`completed` / `succeeded` — реальный продуктовый баг.** CRM Events теперь
   нормализует сохранённый `completed` в публичный `succeeded` на прикладной
   границе, включая replay. Хранилище и старые Core jobs не переписываются.
   Константы и OpenAPI закрепляют единственный успешный статус. Процессный helper
   больше не принимает два варианта и отвергает неизвестные состояния. Прежний
   permissive helper скрывал баг, поэтому прежний PASS не доказывал работу UI.
   Исправлен также оставшийся таймер после ручной успешной проверки операции.
2. **Мёртвый каталог заменён действующей регистрацией.** Registry выбирает реальные
   Activity/Events/Gateway порты для Bridge и RPC-серверов, проверяет интерфейсы,
   режимы, зависимости, локальное владение пулом/квоты и readiness; закрывается
   после сборки. Defaults конфигурации берутся из того же каталога.
   Management `GET /components` показывает фактические bindings и размещение.
   Core policy/OAuth остаются существующими Core facilities, поставляемыми Gateway.
   `contract_version=v1` отделён от `product_version=v0`; переименование wire
   версии из-за названия продукта не требовалось.
3. **Пробелы в доказательствах закрыты отдельными тестами**, перечисленными ниже.
   `make activity-test` включает новые Go-проверки и UI-тесты с ответом реального
   PostgreSQL-получателя; без файла ответа два receiver-dependent UI-кейса
   явно пропускаются, а не считаются успешными.

## Фактически выполнено после исправлений

| Проверка | Результат и артефакт |
| --- | --- |
| `gofmt`, `go vet ./...`, `go test -race -count=1 -v -timeout=15m ./...` с тремя тестовыми DB | exit 0; 258 top-level PASS, 7 SKIP, 0 FAIL; [полный лог](activity-v0-audit-suite.txt) |
| UI Node 22, ответ из `TestOperationSuccessContractPreservesCompletedStorage` | 5 PASS, 0 SKIP; scheduled/manual completion останавливает polling и refresh выполняется один раз; [лог](activity-v0-audit-ui.txt) |
| `TestComponentProcessesAndModeSwitch` с текущими бинарями | PASS 85.41 s; embedded → grpc → embedded, DB CONNECT isolation, durable delivery, Core restart, реальный lead-status против synthetic upstream; [лог](activity-v0-audit-process.txt) |
| `TestComponentOSProcessFaults` | PASS 67.82 s; четыре аварийных сценария; [лог](activity-v0-audit-faults.txt) |
| Сохранённый Core API до исправлений + текущие Activity/Events | PASS 84.00 s; [лог](activity-v0-audit-version-skew.txt), [происхождение бинаря](activity-v0-pre-audit-api.json) |
| Новые production Docker images: API/worker/Activity/Events | build exit 0; Compose smoke PASS; [HTTP-ответы, runtime catalog и image IDs](activity-v0-audit-health.txt) |

Снимок итоговых исходников/конфигураций: [новый SHA-256 manifest](activity-v0-audit-source-manifest.json).
Старый manifest сохранён для исходного отчёта. Go suite выполнен до добавления
опционального выбора сохранённого Core API в harness; этот выбор отдельно проверен
последующим mixed-version прогоном. После сборки образов менялись только документация
и скрипт health smoke, исполняемый на хосте.

Health smoke исполнил реальные HTTP-запросы: начальные Core ready 200 и graph ready
200; после остановки Activity и перезапуска API — Core 200, graph 503; после
восстановления — оба 200. `/components` проверен у API и трёх service owners:
remote bindings не владеют локальными пулами, owner bindings имеют свои квоты.
В конце все пять runtime-контейнеров healthy; стек оставлен в grpc-режиме.

Семь SKIP общего suite: два process controllers, три helper subprocess entrypoints
и два opt-in queue performance benchmarks. Оба controllers реально запущены
отдельно, helpers — их дочерними процессами. Два старых benchmark не перезапускались.
Приведённые количества — top-level тесты, subtests не прибавлены к ним.

Полный suite включает настоящий PostgreSQL + mTLS parity для CRM Events
`Apply`, `Status`, `Operation`: success/replay/conflict, восемь denial-сценариев,
актуальная policy, чужие scope/actor, forged/expired context. Отдельно закрывается
реальное gRPC-соединение после commit получателя; reconnect/replay оставляет
одну inbox/operation/job. Recovery фиксирует 21 день простоя, реконструкцию ещё
через пять дней с прежним окном/page 2 и запрет закрывать старую дыру свежим backfill.

Аварийные сценарии запускают разные OS-процессы и реальные `cmd/crm-events`:

- Два Events PID; первый держит HTTP-страницу, второй перехватывает истёкший lease.
  Поздний ответ первого действительно обработан, stale-запись отсутствует;
  два scheduler ticks оставляют одну job для due source.
- SIGKILL после 100 committed events/page 2; restart после настоящего 30-секундного
  lease продолжает прежние границы/page 2; 500 unique / 1000 processed.
- Helper с настоящим CRM Events service коммитит команду и получает SIGKILL до
  ответа RPC. После запуска обычного сервиса replay возвращает ту же operation;
  одна inbox/operation/job, изменённый payload даёт conflict.
- SIGKILL Gateway/policy останавливает прогресс; restart восстанавливает bounded
  retry. После pilot-disable источник paused за 153 ms, новые calls/data/progress
  отсутствуют весь наблюдаемый интервал 15 s; явный sync возобновляет сбор.

В процессных функциональных тестах Gateway — test executable с настоящими
Gateway/Core/amoCRM-client модулями и синтетическим upstream. Это не функциональный
E2E `cmd/worker` с живым amoCRM. Настоящий `cmd/worker` проверен запуском Compose.
Нагрузочные цифры относятся к локальному fixture, не к production SLO.

Проверка разных версий ограничена конкретным сохранённым Core API:
SHA-256 `4eb3091a8e53c05d6600a74bc3d150d6efa3169436ced4960409cf63a4f80f6e`,
Go 1.25.12 linux/arm64, production non-race. Остальные компоненты и controller
собраны текущим кодом с `-race`. В grpc-фазах использован этот Core API, в
embedded-фазах — текущий. Это не проверка старого Gateway или произвольных релизов.

## Уточнения и оставшиеся ограничения

- Единственный consumer — Activity. Независимое отключение нескольких consumers
  **не реализовано и не доказано**. Второй потребует изменения schema, policy
  и выбора потребностей сборщиком. Это остающаяся граница общего CRM Events,
  а не утверждение о готовой платформе любых потребителей.
- Базовый Compose/Core-only `off` сохранён. Общий Gateway budget доказан для
  включённых Activity-топологий; старый прямой OAuth account lookup в `off`
  не перенесён к worker. Это нельзя описывать как охват всех Go-режимов.
- Событие вне окна останавливает источник консервативной ошибкой. Автоматическое
  пропускание такого события само по себе не доказывало бы полноту диапазона.
- Retention удаляет до 1000 строк глобально за порцию; справедливость очистки
  между installations не доказана. Один Gateway и общий PostgreSQL остаются
  общими точками отказа; GC receipt/inbox отсутствует.
- Все endpoints v0 требуют live admin. Чтение обычным сотрудником собственной
  активности не реализовано; это более узкий продуктовый доступ.
- Установленный виджет, живые OAuth/JWT/CORS/CSP, новое событие реального аккаунта,
  production load/HA и backup реальных данных не проверены. Synthetic restore
  из исходного отчёта не заменяет эти проверки. Нового commit/CI нет.

## Воспроизведение

Smoke уже собранного development grpc-стека с обычными портами (кратковременно
останавливает Activity и перезапускает API, сохраняет runtime DB):

```sh
python3 deploy/activity/verify-health.py
```

`make activity-test` поднимает отдельный development PostgreSQL, готовит только
тестовые БД, выполняет owner/RPC/process suites с race и затем пять Node-тестов.
Команда destructive только для DB с суффиксом `_test`; runtime DB не сбрасываются.

Для полного Go suite после подготовки тестовых БД:

```sh
docker-compose -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  run --rm --no-deps component-tests -race -count=1 -timeout=15m -v ./...
```

Для конкретного сохранённого Core API бинарь должен соответствовать архитектуре
контейнера. В этом workspace он лежит в `tmp/activity-v0-evidence/pre-audit-api`;
в git он не включён. Без этой опции тест явно сообщает, что version skew пропущен:

```sh
docker-compose -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  run --rm --no-deps \
  -e COMPONENT_PROCESS_PREVIOUS_API_BINARY=/src/tmp/activity-v0-evidence/pre-audit-api \
  component-tests -race -count=1 -timeout=7m -v \
  -run '^TestComponentProcessesAndModeSwitch$' ./internal/componentruntime
```
