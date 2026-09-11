# ADR-0022: численные SLO пилота Activity и наблюдаемость

- **Status:** Accepted for the local/pilot contract. Target Grafana and
  Alertmanager are not deployed.
- **Date:** 2026-09-11.

Дополняет [ADR-0006](0006-webhook-payload-retention-and-metrics.md),
[ADR-0009](0009-service-modules-and-fair-admission.md),
[ADR-0010](0010-activity-v0-service-ownership.md),
[ADR-0015](0015-activity-presentation-contract.md) и
[ADR-0016](0016-technical-history-retention.md).

## Контекст

Метрики owner/Core/Gateway уже существовали. OPS-01 требует численных SLO/SLI,
dashboard и alert rules. Целевой Grafana/Alertmanager в этой задаче нет. Нельзя
выдумывать production-ёмкость: числа берутся из REL-02 и из уже принятых
лимитов admission.

Стенд измерений этапа 6: Docker 2 CPU / ~4 GiB, Postgres `mem_limit: 512m`.
Это класс стенда, не доказанная ёмкость целевого хоста.

## Решение

### Контракт объёма до нагрузочного прогона

Это **предварительный контракт пилота**, не измеренная production-ёмкость.

| Параметр | Контракт | Основание |
| --- | --- | --- |
| Установки / аккаунты | 1 installation / 1 account | Первый пилот |
| История | default `retention_days=7` (настройки 2–30 не менять) | ADR-0016, Settings |
| Сотрудники на панель | ≤100 | directory/query limit |
| Concurrent widget readers | 8 | `WIDGET_INSTALLATION_RATE_PER_SECOND=10`, burst 20, gRPC `MaxConcurrent=32`; консервативно ниже in-flight при Panel P95 1.2s |
| Объём событий | envelope REL-02, не «миллионы» | `small_department` 2 240 … `many_employees` 35 000 строк / 7 суток / 100 сотрудников |

Живой amoCRM и целевой хост этот контракт не подтверждают.

### Численные цели (SLI → SLO)

Пороги REL-02 **не пересматривались**. Где прогон быстрее порога, SLO остаётся
порогом стенда, а не «улучшенным» production-числом.

| SLI | SLO пилота на классе стенда 2 CPU / ~4 GiB | Источник | Статус |
| --- | --- | --- | --- |
| Owner Query P95 | ≤ 1.0 s (P99 ≤ 2.0 s) | REL-02 thresholds; many_employees compact 146 ms / full 239 ms | измерен стенд |
| Activity Panel P95 | ≤ 1.2 s (P99 ≤ 2.5 s) | REL-02; many_employees 117 ms; transport HTTP near-bound 69 ms | измерен стенд |
| Owner GetEvent P95 | ≤ 200 ms (P99 ≤ 500 ms) | REL-02; факт < 1 ms | измерен стенд |
| Activity EventCard P95 | ≤ 300 ms (P99 ≤ 700 ms) | REL-02; transport card 2.2 ms | измерен стенд |
| Pool acquire P95 | < 100 ms при `MaxConns=8` | REL-02 пул | измерен стенд |
| Размер JSON | ≤ 3 MiB иначе `resource_exhausted` | `serviceapi.MaxResponseBytes`; L-03 last OK 3 088 807 байт | измерен контракт |
| gRPC frame | < 4 MiB | `servicerpc.MaxMessageSize` | измерен контракт |
| Лаг новых событий | collector lag ≤ 15 s; raw event P95 ≤ 15 s; detail ready P95 ≤ 15 s | L-01 worker criteria; факт lag 4.3 s, raw P95 2.2 s, detail P95 2.9 s на fixture 20 rps / 20 ms | измерен fixture, не живой amoCRM |
| Скорость backfill | 5 backfill × 200 событий + 30 новых за 6.8 s на 2 workers / 20 rps fixture | `fixes/rel-02/workers.json` | измерен fixture; live API — **open** |
| Restore / RTO / RPO | не задан | synthetic `verify-backup.sh` PASS без записанного времени; OPS-02 | **not measured — open** |
| Диск хоста | < 10% free → алерт, если есть node_exporter | в стеке Activity node_exporter нет | **not measured — open** |

Операторский stall (алерты, не SLO чтения): oldest job / outbox > 15 m при
ненулевой очереди. На fixture возраст jobs был секунды.

### Метрики и labels

Добавлены только недостающие ряды с конечными labels:

- `crm_events_coverage{state}` — `unknown`/`partial`/`verified`/`other`;
- `crm_events_coverage_gap_seconds` — возраст newest coverage window, без ID;
- `activity_http_response_bytes{route}` — `panel`/`event`/`other`, buckets
  включают 3 MiB;
- `component_db_size_bytes{service}` / `component_db_size_up{service}` —
  `pg_database_size(current_database())`, timeout 2 s, `up=0` без нулевого
  размера.

ID установки/аккаунта/пользователя, пути и payload запрещены как labels.
Детализация по аккаунту остаётся в защищённой диагностике виджета.

### Dashboards и алерты

Артефакты лежат в `deploy/observability/`. Overlay
`docker-compose.activity-observability.yml` только опционально скрейпит уже
существующие management listeners. Grafana-сервер и Alertmanager целевой среды
**не** входят в это решение.

## Последствия

- Пилот можно наблюдать локально, не утверждая production-ёмкость.
- Restore, живой backfill amoCRM и диск хоста остаются открытыми (OPS-02 / QA-02).
- Смена SLO без нового измерения на том же классе стенда запрещена.
