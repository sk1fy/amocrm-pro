# Activity v0: исправления второго аудита

Дата: 2026-09-07. База изменений — `d8524cb`. Этот отчёт дополняет
[исторический отчёт первого аудита](activity-v0-audit-followup.md).
Пользователь сообщил об успешной проверке предыдущей версии в установленном
виджете amoCRM. Новые исправления на сервер пока не устанавливались; это сообщение
не является E2E-проверкой новой версии.

## Реализовано

- H1: обязательный CI job с тремя тестовыми БД, owner/RPC/OS-process/UI проверками;
  отдельный Compose-проект и отказ при пропущенных обязательных тестах. Новые
  образы добавлены в build matrix, все Activity Compose overlays валидируются.
  Явный `-p` защищает development/test запуск от унаследованного
  `COMPOSE_PROJECT_NAME`; disposable имя обязано оканчиваться на `-test`.
- H2/M1: один live actor lookup на новый Issue, текущая Core DB policy при каждом
  Validate; минимальные grants входящего запроса и отдельной доставки. Activity
  identity больше не может вызвать CRM Events Apply. Окно отзыва amoCRM-роли
  явно изменено: текущая делегация действует до 30 секунд, новый Issue проверяет
  роль заново. Capability/pilot/installation/reauth не кешируются. Подробности —
  [ADR-0011](../adr/0011-activity-authorization-budget.md).
- H4/M3: атомарный retention frontier, чередование источников и индексируемый
  отбор; статус, события и агрегаты читаются в одном PostgreSQL snapshot.
  SavePage делает один SELECT хешей и один pgx batch вместо двух обменов с БД
  на событие, сохраняя счётчики повторяющихся ID и порядок блокировок.
- M2/M4: фоновые ошибки логируются конечными категориями без raw SQL/payload/
  секретов. Events ограничивает повтор одного stage/code разом в минуту.
  Core outbox последовательно дренирует готовую работу, ожидая секунду только
  при пустой очереди или ошибке; сохранены durable retry и общий Gateway budget.
  Запись уже полученной страницы переживает отмену родительского контекста,
  ограничена пятью секундами и по-прежнему требует действующего lease/fencing.
- M6/M7: общий retention 2–30 дней в owner application/schema, ошибка вместо
  ложного succeeded при несовпадении scope настроек, откат квитанции этой команды;
  сбой capability query возвращает 503/unavailable вместо ложного 403.
  Первая sync при недоступных настройках не создаёт receipt/outbox, повтор
  уже принятой команды использует прежний снимок настроек.
- M8: `restart: unless-stopped` у пяти долгоживущих контейнеров development-стека;
  служебные миграторы/генератор сертификатов/CLI не перезапускаются бесконечно.

CRM Events алгоритмы, тесты и ограничения подробно описаны в
[отчёте владельца Events](activity-events-fixes.md); CI — в
[инструкции проверок](activity-ci.md).

## Проверки

После проверки и объединения изменений трёх субагентов выполнены:

| Проверка | Фактический результат |
| --- | --- |
| `gofmt -l .`, `go vet ./...` в Docker Go 1.25 | exit 0 |
| Полный `go test -race -p 1 -count=1 -v -timeout=15m ./...` с Core/Activity/Events test DB | 275 top-level PASS, 7 SKIP, 0 FAIL; [лог](activity-v0-hardening-go.txt) |
| `make COMPOSE=docker-compose activity-ci` на свежем disposable PostgreSQL 17 | exit 0; 93 top-level PASS, 3 helper SKIP, 0 FAIL; [компоненты](activity-v0-hardening-components.txt) |
| Node 22 UI с operation JSON, записанным настоящим PostgreSQL receiver test | 5 PASS, 0 SKIP; [лог](activity-v0-hardening-ui.txt) |
| `git diff --check` и Compose configuration | exit 0 |

Полный Go suite пропускает два opt-in process controllers, три subprocess helpers
и два прежних queue performance benchmarks. Controllers реально выполнены в
`activity-ci`; там только три ожидаемых helper entrypoints пропускаются в родителе
и запускаются дочерними процессами. Суммы двух Go suite перекрываются, их нельзя
складывать как количество разных тестов. Queue benchmarks не перезапускались.

`TestComponentOSProcessFaults` — PASS 67.95 s: два Events PID/fencing, SIGKILL после
commit страницы, SIGKILL получателя до ответа RPC, отказ Gateway, restart и
измеренное прекращение сбора после pilot-disable. `TestComponentProcessesAndModeSwitch`
— PASS 84.50 s: реальные API/Activity/Events процессы, owner DB isolation,
embedded → grpc → embedded, durable delivery и прежний lead-status.
В совместной нагрузке выполнено 60 настоящих lead PATCH против synthetic amoCRM;
completion P95 idle=1.135085 s, loaded=1.139283 s, errors=0, retries=0. Это локальная
проверка с synthetic upstream и Gateway fixture, не production SLO.

Новый production-checker тест измерил два panel calls → два actor HTTP GET
(один на запрос); первый panel дополнительно делает два directory GET,
второй использует кеш справочника. Проверены live DB revocation, bounded роль
после её отзыва, истечение делегации и отказ mTLS Activity→Events Apply.
Новая миграция проверена на свежей БД и отказе legacy retention без изменения
данных; атомарность retention проверена инъекцией сбоя и конкурентным чтением.

Тестовый Compose-проект и его volumes удалены после прогона. Существующий локальный
runtime-стек не пересоздавался, его пять контейнеров остались healthy. Новая
GitHub Actions job добавлена, но удалённый CI не запускался (нового commit/push нет).
Совместимость разных версий бинарей в этом прогоне не проверялась: optional
previous-API artifact не передавался. Прежний результат относится к своей версии.

## Перед установкой на сервер

Новая миграция CRM Events — `000002_retention_consistency`. Её выполняет только
владелец Events DB. Перед обновлением проверить источники с retention вне 2–30
дней: миграция намеренно отклоняет их, не сокращая хранение молча. SQL проверки
есть в отчёте Events. Старый Go-код не использует новые колонку/индексы, но перед
возвратом приложения надо учитывать суженный CHECK; down-миграция не возвращает
удалённые данные. Не применять автоматический rollback миграций.

Использовать production-оверлей существующего Core, а не переключать публичный
nginx на пустую Core DB демонстрационного стека. Обновить owner migration и
совместимые API/worker/Activity/CRM Events binaries с прежними DSN/identities.
Новые сертификаты или перенос OAuth для этих изменений не требуются.
После установки проверить bootstrap, panel, sync до succeeded и прежний ping.

## Решения по остальным пунктам аудита

- H3: некорректный ответ по-прежнему не продвигает coverage. Добавлена проверка
  восстановления новой sync в том же окне после исправления upstream-ответа.
  Постоянно некорректное окно остаётся явной ошибкой; skip/quarantine и изменение
  created_at не добавлены: без модели разрывов это дало бы ложную полноту истории.
- M3: короткий advisory lock Claim сохранён для общего ограничения backfill.
  Удаление блокировки без эквивалентного quota-протокола небезопасно. Batch
  сокращает сетевые обмены, а не число отдельных серверных INSERT; production
  latency/fairness SLO из этого не выводятся.
- M5: срок сохранения receipt/outbox/inbox/operations/jobs v0 остаётся
  неограниченным. Это явный dedup/retry horizon текущего контракта: GC пока
  выключен. Сокращение срока требует общего ограниченного срока повторов и
  изменения операторского retry; старые команды нельзя молча превратить в новые.
  Рост технической истории остаётся эксплуатационным ограничением.
- M6: следующая принятая enable/sync/backfill использует снимок настроек Activity;
  backfill продолжает применять этот снимок к источнику. Это сохранённая
  семантика, а не скрытая коррекция существующих команд.
- M1/M8: глобальное одноразовое потребление delegation jti, произвольный leeway
  и нечувствительность ACTIVITY_MODE к регистру не добавлялись. Межсервисная
  делегация штатно используется на нескольких переходах, часы Policy едины,
  режимы задаются явным enum. `/components` остаётся management-only; listener
  требуется защищать loopback/закрытой сетью, не публиковать через общий ingress.
- Низкоприоритетные замечания о прежних limiter maps без eviction, неиспользуемых
  индексах и дополнительном клиентском лимите поверх серверного response budget
  в этот набор изменений не включены.

История исправлений и живой проверки хранится по датам. Старые артефакты не
переписываются как доказательства тестирования нового кода.
