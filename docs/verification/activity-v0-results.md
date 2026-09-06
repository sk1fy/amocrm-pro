# Activity v0: реализация и фактическая проверка

> Это исходный отчёт первого прогона. Найденные аудитом ошибки, уточнение границ
> доказательств и новые проверки описаны в [дополнении после аудита](activity-v0-audit-followup.md).

Дата: 2026-09-06. Исходный commit: `a33a845f2db23b46c96fc7199d1161f654ad86f5`,
ветка `main`. Изменения находятся в рабочем дереве; новый commit не создавался.
Актуальное задание — архитектурный summary **v2**. Результаты ниже относятся к
локальному Docker-стенду и тестовым данным, не к серверу или production cutover.

Снимок исходников и конфигураций: [SHA-256 manifest](activity-v0-source-manifest.json),
идентификатор набора `529cbbeb44622ac7342bbb4dc874801cd2c048733612b9b045f7f4ec9eaa640f`.

## Реализовано

- Core сохраняет OAuth, encrypted credentials, widget JWT/CORS, capabilities,
  rate admission и прежний lead-status. Добавлены installation pilot, отдельные
  receipts/outbox с атомарным потреблением JWT и audited enable/disable/retry.
- CRM Events владеет отдельной logical DB: история, consumer, inbox, operations,
  jobs и coverage. Прикладной слой использует собственный Repository и Policy/
  Gateway порты. Есть fixed windows, страницы по 100, bounded stabilization,
  atomic page/cursor/continuation, source leases/fencing, retries, reauth, restart,
  backfill priority и retention без отката проверенного прогресса.
- Activity владеет отдельной logical DB настроек/квитанций, читает CRM Events
  пакетно и возвращает события, уникальные количества, последнее событие и
  явную свежесть данных. Добавлен frontend-адаптер для существующего виджета.
- Typed protobuf, local/gRPC адаптеры одной бизнес-логики, mTLS service identities,
  Core-only Ed25519 delegation, live admission/admin policy, bounded deadlines,
  concurrent RPC quotas, health, SQL/pool/queue/RPC/outgoing metrics.
- Отдельные `activity`/`crm-events` binaries и контейнеры, owner migrations,
  runtime/migration roles, Compose для embedded/grpc, runbooks переключения,
  отключения, диагностики и восстановления. Core `/ready` не зависит от Activity;
  граф проверяется отдельно через `/components/activity/ready`.
- Один существующий Core worker владеет Gateway и общим amoCRM client/limiter.
  Lead-status, webhook reconciliation и Events используют этого владельца.
  Собственные сервисы не получают OAuth credentials или чужие DSN.
  OAuth API v4 account lookup направлен в Core-only Bootstrap RPC того же
  владельца; обмен/обновление токенов сохранили прежний Core механизм.
- Прикладной JSON-ответ ограничен 3 MiB в обоих режимах, gRPC frame — 4 MiB.
  Directory cache дополнительно ограничен 512 KiB и длинами полей.

Архитектура: [ADR-0010](../adr/0010-activity-v0-service-ownership.md).
Запуск: [runbook](../runbooks/activity-v0.md).

## Проверено автоматически

Все следующие команды реально выполнялись; exit code 0:

| Проверка | Результат |
| --- | --- |
| `make test` до изменений | Baseline format, vet, race unit tests прошли |
| `make integration-test` с новой Core migration | Core/widget PostgreSQL suites, guarded rollback и конкурентные миграции прошли |
| Docker: `test -z "$(gofmt -l .)"; go vet ./...; go test -race -count=1 -v -timeout=15m ./...` с тремя test DSN | **247 top-level tests PASS**, FAIL 0 |
| Docker Node 22: `node --test examples/activity-v0/panel.test.mjs` | 3 PASS, 0 FAIL |
| Docker PostgreSQL 17: `sh deploy/activity/verify-backup.sh` | Synthetic owner `pg_dump/pg_restore`: события, coverage, source progress, consumer, jobs и inbox/operation identity сохранены |
| Compose config: grpc, embedded override, test override | Все три конфигурации валидны |

В полном Go suite четыре top-level SKIP: process test и его fixture запускаются
отдельно, а `TestFairClaimPerformance` и `TestWorkerPollingPerformance` требуют
своих opt-in benchmark flags и в этой работе не запускались. Их нельзя считать
пройденными. Обычные существующие проверки fair claims/leases/widget isolation
в PostgreSQL suite выполнялись.

Один промежуточный повтор полного suite упал в существующем
`TestStoreBoundsExpiredLeaseReapingAt100kBacklog`: EXPLAIN оценивал 100 000 строк
как одну и выбирал другой индекс/сортировку на унаследованной статистике.
После bulk seed в фикстуру добавлен `ANALYZE jobs`; исходная проверка требуемого
индекса сохранена, production queue SQL не менялся. Три повтора этого теста и
затем весь окончательный suite прошли. Промежуточное падение не засчитано как PASS.

Контрактные/failure проверки включают forged/expired/wrong-audience/nonadmin
context, чужую installation/integration/actor, local/gRPC parity и oversized
responses, повтор/конфликт команды, потерю ответа после commit, recipient outage,
истечение/fencing lease, два scheduler/worker, rollback partial page, пустые окна,
одинаковые timestamps, изменение пагинации, reauth/policy outage и retention.

DB suite использовал `core_components_test`, `activity_components_test`,
`events_components_test` в отдельном Docker PostgreSQL. `TEST_DATABASE_RESET_ALLOWED`
был включён только для этих expendable test DB. Непроверенный production backup
не подменяется успешным synthetic restore.

## Проверено отдельными процессами

`TestComponentProcessesAndModeSwitch` реально собирает и запускает `cmd/api`,
`cmd/activity`, `cmd/crm-events` и отдельный Gateway/policy/worker fixture. Последний
использует настоящие Core policy, Gateway, amoCRM client/limiter и существующий
leadstatus jobs module; только amoCRM HTTP ответы и token provider синтетические.

Фактические pools стенда: Core API 5, Gateway/worker 4, Activity 3, Events 5.
Core workers 2, Events workers 2, одна page lease на источник и одна backfill
page глобально. Widget admission rates в workload увеличены до 1000/1000,
чтобы измерять влияние компонентов отдельно от ingress throttling. Development
Compose имеет собственные явно заданные pools 6+6+3+5.

Подтверждено:

- отказ CONNECT во всех шести сочетаниях runtime-роли с чужой owner DB;
- standalone subprocess env содержит только собственные DSN/identities;
- полезная панель, настройки, события и command/operation IDs сохраняются при
  **embedded → grpc → embedded** на тех же owner DB;
- Core перезапускается, остаётся ready и выполняет widget ping при остановленной
  Activity; Core `202` сохраняет outbox, доставка восстанавливается после старта;
- существующий lead-status job выполняется через тот же client/budget;
- отключение pilot блокирует новый доступ и работу.

Окончательный процессный прогон с `-race` (включая все собранные subprocess binaries)
завершился за **93.864s**, exit 0. При backlog 10 000 событий / страница 100 /
два collector slots выполнено по **30 настоящих lead-status jobs** в idle/loaded
фазах, два closed-loop callers и отдельные lead IDs. Подтверждены 60 PATCH-эффектов,
0 ошибок и 0 retries; backfill оставался активным до и после каждого loaded job.

| Lead-status | Idle P95 / P99 | Сбор включён P95 / P99 |
| --- | ---: | ---: |
| Приём HTTP 202 | 12.669 / 30.149 ms | 28.682 / 45.764 ms |
| Durable завершение job | 1.139815 / 1.156184 s | 1.174215 / 1.281703 s |

Core job backlog после каждой фазы — 0. Дополнительно выполнено 50 ping в каждой
фазе с concurrency 2:

| Измерение | Idle | Сбор включён |
| --- | ---: | ---: |
| Ping P95 | 26.904 ms | 15.505 ms |
| Ping P99 | 57.797 ms | 25.017 ms |
| Ошибки | 0 | 0 |
| Core empty acquire count | 3 | 3 |
| Core canceled acquire count | 0 | 0 |
| Core cumulative acquire duration | 0.120396035 s | 0.120975275 s |

Снимок Events: 10 000 inserted, 10 602 processed, 602 deduplicated и 1 running job;
oldest job 16.984s, lag 2.001s. Полная backfill operation завершилась
до смены режима. Снимок Gateway: 480 ответов 2xx, суммарное ожидание бюджета 52.579s;
выборки двух integrations содержали 296 Events requests, 121 lead request и 60
lead PATCH, интервалы удовлетворяли
burst 7 + 7 rps с оговорённым допуском планирования.

Тот же process run проверил Core-only Bootstrap account RPC для двух integrations
одного account и запрет этого RPC обеим продуктовым identities. Отдельные тесты
подтвердили общую account/integration квоту с обычными вызовами, discovery debit,
отмену/429, актуальный integration policy и callback без локального fallback.

Пределы зафиксированы в [плане](activity-v0-plan.md) до запуска. Эти цифры —
локальный bounded fixture benchmark. Они не доказывают production SLO, полную
физическую изоляцию или latency при произвольной production-нагрузке.
Полный stdout: [process evidence](activity-v0-process.txt). Воспроизведение:

```sh
docker run --rm --network amocrm-activity_default \
  -v "$PWD:/src" -v amocrm-pro-gomod:/go/pkg/mod \
  -v amocrm-pro-gobuild:/root/.cache/go-build -w /src \
  -e COMPONENT_PROCESS_TEST_ADMIN_URL='postgres://pilot_admin:pilot_admin_dev@postgres:5432/postgres?sslmode=disable' \
  -e COMPONENT_PROCESS_TEST_ALLOWED=true golang:1.25 \
  go test -race ./internal/componentruntime \
  -run '^TestComponentProcessesAndModeSwitch$' -count=1 -v -timeout=7m
```

Дополнительно реально выполнены build всех runtime images и Compose grpc startup;
все пять runtime containers healthy. В отдельном процессе Activity окружение
содержало ACTIVITY_DATABASE_URL, в Events — CRM_EVENTS_DATABASE_URL, у обоих только
свой `/identity` volume. Проверен Core restart при остановленной Activity:
Core `/ready`=200, `/components/activity/ready`=503. Compose embedded startup и
возврат grpc также дали ready=200 на тех же volumes. В этой Compose DB нет
настоящей выбранной installation; полезные данные проверены process fixture.

## Проверено в amoCRM

**Не выполнено.** Выбранная реальная installation и код установленного виджета
не были предоставлены. Запрос к официальной документации проверил контракт API,
но не является OAuth, browser или Events E2E. Frontend adapter не установлен
в пользовательскую integration.

## Не проверено

- Новое реальное CRM-событие → сбор → установленный browser widget, включая
  реальные права, CORS/CSP, asset loading, OAuth/reauth и административное отключение.
- Production deployment/release, production P95/P99, server capacity, HA и
  несколько Gateway replicas; restore настоящих данных/реального инстанса.
- PHP history migration, canary/cutover и migration rollback: вне Activity v0.
- Два opt-in Core performance benchmarks, отмеченные SKIP выше; CI нового commit
  не запускался, поскольку commit/push не выполнялись.

## Оставшиеся ограничения

- Один worker/Gateway — владелец Go API v4 исходящего бюджета, включая OAuth
  account lookup. PHP, сторонний трафик и token exchange/refresh
  `/oauth2/access_token` не входят в него. Первый ещё неизвестный account проходит
  discovery квоту до определения canonical ID. Общий PostgreSQL и Gateway
  остаются общими зависимостями.
- Стабильные повторные API scans не гарантируют immutable snapshot или доступ
  ко всей истории. Очень поздние события за пределами overlap требуют backfill.
- V0 имеет только consumer Activity и требует admin-role для всех её endpoints.
  Настройки применяются к сбору следующей sync-командой.
- Receipt/inbox/job history не очищается автоматически; перед долгой эксплуатацией
  нужен согласованный retry/dedup retention horizon. Ограниченный metrics scrape
  может вернуть up=0 при слишком большой lifetime history.
- Frontend — подключаемый адаптер с bounded polling, не marketplace archive.
  Рабочие сессии, паузы, сценарии, Telegram, billing и сложные отчёты не добавлялись.

Реализация и локальная отделяемость проверены; **реальный пилот Activity v0 ещё
не принят** до выполнения установленного-widget amoCRM E2E.
