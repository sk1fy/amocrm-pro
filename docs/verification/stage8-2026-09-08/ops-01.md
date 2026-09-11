# OPS-01: наблюдаемость и SLO

Дата: 11 сентября 2026. Задача плана: этап 8, OPS-01.

Локальные артефакты и численный контракт пилота. **Целевой Grafana,
production Alertmanager и удалённый сервер не трогались и не отмечаются
выполненными.** REL-02 не перезапускался; использованы уже записанные
измерения `docs/verification/stage6-2026-09-08/`.

## Итог

| Пункт плана | Результат |
| --- | --- |
| Целевые объёмы до нагрузочного прогона | Зафиксированы как **контракт пилота**, не как production-ёмкость |
| Численные SLO/SLI | Взяты из REL-02 / L-01 worker / admission limits. Restore и live backfill — open |
| Dashboard | Импортируемый JSON, не живой на целевом хосте |
| Алерты | Rules-файл для локального Prometheus; Alertmanager пустой |
| Bounded labels | Новые ряды без installation/account/user/path/payload |

## Контракт объёма до нагрузки

Не доказанная ёмкость целевого хоста.

| Параметр | Значение | Основание |
| --- | --- | --- |
| Аккаунты | 1 installation / 1 account | Первый пилот |
| История | default 7 дней (диапазон настроек 2–30 **не** менялся) | Settings / ADR-0016 |
| Панель | ≤100 сотрудников | query/directory limit |
| Concurrent widget readers | 8 | `WIDGET_INSTALLATION_RATE_PER_SECOND=10`, burst 20, RPC slots 32; консервативно |
| События | envelope REL-02: 2 240 (`small_department`) … 35 000 (`many_employees`) строк | не «миллионы» |

Стенд измерений: Docker 2 CPU / ~4 GiB, Postgres 512 MiB.

## SLO

| SLI | SLO | Источник | Статус |
| --- | --- | --- | --- |
| Query P95 / P99 | 1.0 s / 2.0 s | REL-02 `thresholds.json`; факт many_employees 146 / 239 ms | измерен стенд |
| Panel P95 / P99 | 1.2 s / 2.5 s | REL-02; факт 117 ms | измерен стенд |
| GetEvent P95 / P99 | 200 ms / 500 ms | REL-02; факт <1 ms | измерен стенд |
| EventCard P95 / P99 | 300 ms / 700 ms | REL-02; transport 2.2 ms | измерен стенд |
| Pool acquire P95 | <100 ms при MaxConns=8 | REL-02 | измерен стенд |
| JSON / gRPC | 3 MiB / 4 MiB | L-03; last OK JSON 3 088 807 | измерен контракт |
| Лаг новых событий | ≤15 s (collector / raw / detail ready) | L-01 worker criteria; факт lag 4.3 s, raw 2.2 s, detail 2.9 s на fixture 20 rps / 20 ms | fixture, не живой amoCRM |
| Backfill | 5×200 событий + 30 новых за 6.8 s, 2 workers | `fixes/rel-02/workers.json` | fixture; live API — **not measured — open** |
| Restore RTO/RPO | не задан | synthetic `verify-backup.sh` без времени; OPS-02 | **not measured — open** |
| Диск хоста | 10% free, если есть node_exporter | в стеке нет node_exporter | **not measured — open** |

Операторский stall (алерты, не read SLO): oldest job/outbox >15 m при ненулевой очереди. На fixture возраст jobs — секунды.

## Инвентарь метрик

| Имя | Было / добавлено | Labels | Панель dashboard |
| --- | --- | --- | --- |
| `crm_events_jobs` | было | `state` | Jobs and Core outbox |
| `crm_events_oldest_job_age_seconds` | было | — | Oldest jobs |
| `crm_events_max_lag_seconds` | было | — | Lag and coverage gap |
| `crm_events_events_total` | было | `outcome` | (служебная; не отдельная панель) |
| `crm_events_enrichment` | было | `state` | Enrichment backlog |
| `crm_events_enrichment_oldest_age_seconds` | было | — | Oldest jobs |
| `crm_events_metrics_up` | было | — | Lag and coverage gap |
| `crm_events_coverage` | **добавлено** | `state`=unknown\|partial\|verified\|other | Enabled sources by coverage class |
| `crm_events_coverage_gap_seconds` | **добавлено** | — | Lag and coverage gap |
| `activity_delivery_commands` | было | `state` | Jobs and Core outbox |
| `activity_delivery_oldest_pending_seconds` | было | — | Jobs and Core outbox |
| `activity_delivery_errors` | было | `code` (finite, в т.ч. `reauth_required`, `unavailable`) | Reauth and delivery errors |
| `activity_delivery_metrics_up` | было | — | (алерты outbox) |
| `activity_http_response_bytes` | **добавлено** | `route`=panel\|event\|other; buckets до 3 MiB и 4 MiB | Widget response size |
| `amocrm_budget_wait_seconds` | было | `outcome` | Read P95 and budget wait |
| `amocrm_requests_total` | было | `outcome` (2xx/429/401/4xx/5xx/…) | API errors and 429; Reauth |
| `component_db_connections_*` / `acquire_seconds_total` / `empty_acquire_total` / `canceled_acquire_total` | было | `service` | Pool wait and SQL |
| `component_sql_duration_seconds` | было | `service`,`operation` | Pool wait and SQL |
| `component_db_size_bytes` | **добавлено** | `service` (owner DB, без tenant) | Owner database storage |
| `component_db_size_up` | **добавлено** | `service` | Owner database storage |
| `amocrm_jobs_service_backlog` | было | `service`,`kind` | (алерты/диагностика очереди Core) |
| `amocrm_jobs_service_oldest_ready_seconds` | было | `service` | Oldest jobs |
| `service_rpc_requests_total` | было | `method`,`code` | API errors and 429 |
| `service_rpc_duration_seconds` | было | `method` | Read P95 and budget wait |
| `amocrm_cleanup_passes_total` | было | `outcome` | Cleanup throughput |
| `amocrm_cleanup_rows_deleted_total` | было | `record` | Cleanup throughput |
| `amocrm_cleanup_batch_limit_total` | было | `record` | Cleanup throughput |
| `amocrm_widget_limit_decisions_total` | было | `scope`,`outcome` | API errors and 429 |
| oauthlimit / webhook ingress | было | конечные scope/outcome | не вынесены отдельной панелью пилота |

`crm_events_events_total` и Core job backlog остаются в `/metrics`; на dashboard пилота акцент на lag/coverage/outbox/read/storage/cleanup.

## Что не сделано

- Grafana на целевом сервере не развёрнута, dashboard не live.
- Alertmanager не установлен; `alerting.alertmanagers: []`.
- SSH/remote scrape, node_exporter, host disk — нет.
- `docker-compose.activity.yml` не менялся; overlay опционален.
- REL-02 / activity-ci / compose test stack не запускались.
- RTO/RPO restore — OPS-02.
- Живой amoCRM backfill speed — open.

## Файлы

Созданы:

- `deploy/observability/prometheus.yml`
- `deploy/observability/alerts.yml`
- `deploy/observability/grafana/activity-pilot.json`
- `deploy/observability/README.md`
- `docker-compose.activity-observability.yml`
- `docs/adr/0022-activity-slo-and-observability.md`
- `docs/runbooks/activity-observability.md`
- `docs/verification/stage8-2026-09-08/ops-01.md`
- `internal/services/crmevents/metrics_test.go`
- `internal/componentruntime/database_metrics_test.go`

Изменены:

- `internal/services/crmevents/metrics.go`, `postgres_metrics.go`
- `internal/activitybridge/metrics.go`, `metrics_test.go`, `bridge.go`, `http.go` (observe размера ответа на widget routes; `cmd/api` не требовался, Collector уже регистрируется в API)
- `internal/componentruntime/database.go`

`httpserver/system.go` не менялся: HTTP size вешается на существующий Bridge collector, storage — на pool metrics.

## Команды

```sh
gofmt -w internal/services/crmevents/metrics.go \
  internal/services/crmevents/postgres_metrics.go \
  internal/services/crmevents/metrics_test.go \
  internal/activitybridge/metrics.go \
  internal/activitybridge/metrics_test.go \
  internal/activitybridge/bridge.go \
  internal/activitybridge/http.go \
  internal/componentruntime/database.go \
  internal/componentruntime/database_metrics_test.go
python3 -m json.tool deploy/observability/grafana/activity-pilot.json > /dev/null
python3 -c "import yaml; yaml.safe_load(open('deploy/observability/prometheus.yml')); yaml.safe_load(open('deploy/observability/alerts.yml')); yaml.safe_load(open('docker-compose.activity-observability.yml'))"
go test -count=1 -timeout 60s ./internal/services/crmevents \
  -run 'TestMetricStateMapsUnknownValuesToOther|TestMetricsCollectorUnavailableDoesNotPublishZeroBacklog|TestMetricsCollectorEmitsBoundedCoverageAndUnknownStates'
go test -count=1 -timeout 60s ./internal/activitybridge \
  -run 'TestUnavailableDeliveryMetricsDoNotPublishZeroBacklog|TestHTTPRouteClassIsBounded|TestHTTPResponseSizeUsesBoundedRouteClassAndIncludesCapBucket|TestHTTPResponseSizeDoesNotLogBodies'
go test -count=1 -timeout 60s ./internal/componentruntime \
  -run 'TestDatabaseSizeCollectorUnavailableDoesNotPublishZeroBytes|TestDatabaseSizeCollectorReportsOwnerBytes'
docker-compose -f docker-compose.activity.yml \
  -f docker-compose.activity-observability.yml config --quiet
```

Результаты:

| Команда | Результат |
| --- | --- |
| gofmt + JSON/YAML | ok |
| `go test ./internal/services/crmevents -run TestMetric*` | PASS, 1.525 s |
| `go test ./internal/activitybridge -run TestUnavailable\|TestHTTP*` | PASS, 0.674 s |
| `TestDatabaseSizeCollectorUnavailableDoesNotPublishZeroBytes` | PASS |
| `TestDatabaseSizeCollectorReportsOwnerBytes` | **SKIP** — `TEST_DATABASE_URL is not set` |
| compose config overlay | `compose_ok` |

Compose/activity-ci/integration-test не запускались (коллизия с другими агентами).

Для координатора, когда свободен Postgres test DSN:

```sh
TEST_DATABASE_URL='postgres://…/*_test?sslmode=disable' \
TEST_DATABASE_RESET_ALLOWED=true \
go test -count=1 -timeout 60s ./internal/componentruntime \
  -run TestDatabaseSizeCollectorReportsOwnerBytes
```

Не стартовать compose из этой задачи.
