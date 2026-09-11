# Runbook: наблюдаемость Activity

Связанные документы: [ADR-0022](../adr/0022-activity-slo-and-observability.md),
[артефакты](../../deploy/observability/README.md),
[widget capacity](widget-capacity.md).
Этот файл **не** заменяет [activity-v0.md](activity-v0.md) и не описывает
backup целевого сервера.

Целевой Grafana и production Alertmanager **не установлены**. Ниже —
как пользоваться локальными артефактами и что смотреть при алертах.

## Где метрики

| Процесс | Путь | Compose без overlay | Overlay |
| --- | --- | --- | --- |
| Core API | `GET /metrics` на management | `127.0.0.1:18082` | то же |
| Core worker | `GET /metrics` | только внутри контейнера `:8081` | `worker:8081` по Docker DNS |
| Activity | `GET /metrics` | `:8091` внутри | `activity:8091` по Docker DNS |
| CRM Events | `GET /metrics` | `:8092` внутри | `crm-events:8092` по Docker DNS |

```sh
docker-compose -f docker-compose.activity.yml exec -T crm-events \
  wget -qO- http://127.0.0.1:8092/metrics
docker-compose -f docker-compose.activity.yml exec -T api \
  wget -qO- http://127.0.0.1:8082/metrics
```

Labels конечные. Не искать `installation_id` в Prometheus — его там нет.
Карточка/диагностика виджета показывает coverage/freshness конкретной
установки.

## Импорт dashboard

1. Поднять Prometheus (overlay или свой).
2. В Grafana Import `deploy/observability/grafana/activity-pilot.json`.
3. Datasource = этот Prometheus. UID dashboard: `activity-pilot`.

Не считать импорт на ноутбуке живым dashboard целевого хоста.

## Алерты (локальные rules)

Файл [alerts.yml](../../deploy/observability/alerts.yml) загружается локальным
Prometheus. Alertmanager пустой: страница `/alerts` показывает состояние, писем
и pager нет.

| Алерт | Смысл | Что проверить |
| --- | --- | --- |
| `CRMEventsStuckCollection` | Job >15m и очередь не пуста | `crm-events` logs, leases, `reauth_required` в status, не seq scan |
| `ActivityLongReauth` / `AmoCRMUnauthorizedRate` | Долгий reauth или 401 v4 | OAuth credentials, пилот enable, диагностика установки |
| `GatewayRPCUnavailable` / `ActivityDeliveryUnavailable` | RPC/outbox `unavailable` | `/components`, mTLS, процесс Gateway/Activity/Events |
| `GatewayScrapeUnavailable` | Gateway management не отвечает или цель исчезла из scrape | Core worker, сеть и `up{job="activity-worker"}`; серверный RPC counter при остановке процесса не растёт |
| `ActivityOutboxGrowing` | pending/delivering >15m | Core worker, product Ready, повтор delivery |
| `CleanupStall` / `CleanupSilent` | batch limit без delete или нет completed pass | worker lock, `CLEANUP_INTERVAL` (default 15m) |
| `OwnerDatabaseSizeUnreadable` | `pg_database_size` не собрался | connectivity owner DB, `component_db_size_up` |
| `OwnerDatabaseSizeGrowingFast` | placeholder роста owner DB | не диск хоста; смотреть retention/cleanup |
| `HostDiskLow` | 10% free | **нужен node_exporter**, в overlay его нет |

Порог 15m для stuck/outbox — операторский stall. SLO лага новых событий на
стенде REL-02 — 15s.

## SLI на дашборде

- Лаг: `crm_events_max_lag_seconds` (свежесть), `crm_events_coverage` /
  `crm_events_coverage_gap_seconds` (полнота retained window).
- Чтение: `service_rpc_duration_seconds` для `GetPanel`/`GetEvent`/`QueryEvents`.
- 429: amoCRM v4 `outcome="429"`, widget limiter `rejected`, RPC
  `ResourceExhausted` (в т.ч. ответ >3 MiB).
- Размер ответа: `activity_http_response_bytes{route="panel|event|other"}`.
  Тела не логируются.
- Storage: `component_db_size_bytes{service}` без tenant labels.
- Cleanup: `amocrm_cleanup_*`.

## Кардинальность

Если в `/metrics` появился label с UUID, URL или текстом примечания — это дефект,
не «удобная диагностика». Скрейп не должен размножать ряды на аккаунт.
