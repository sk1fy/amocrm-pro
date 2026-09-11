# Наблюдаемость Activity (локальные артефакты)

Это **не** production Grafana и **не** установленный Alertmanager целевого
сервера. Каталог содержит правила, scrape-конфиг и dashboard, которые оператор
может импортировать в свой Prometheus/Grafana.

## Что есть

| Файл | Назначение |
| --- | --- |
| [prometheus.yml](prometheus.yml) | Scrape management listeners стека Activity по Docker DNS |
| [alerts.yml](alerts.yml) | Recording + alert rules. Alertmanager в конфиге пустой |
| [alerts.test.yml](alerts.test.yml) | Promtool-проверки срабатывания, выдержки `for`, восстановления и изоляции scrape targets |
| [grafana/activity-pilot.json](grafana/activity-pilot.json) | Импортируемый dashboard `uid=activity-pilot` |
| [../../docker-compose.activity-observability.yml](../../docker-compose.activity-observability.yml) | Опциональный overlay: только Prometheus, scrape по Docker DNS |

`docker-compose.activity.yml` без overlay **не** меняется: worker/activity/crm-events
по-прежнему не публикуют management наружу, Grafana-сервер не добавляется.
Overlay **не** вешает host-порты на worker/activity/crm-events: иначе Compose
пересоздаст collector пилота `amocrm-activity`.

## Локальный scrape overlay

```sh
docker-compose -f docker-compose.activity.yml \
  -f docker-compose.activity-observability.yml up -d prometheus
```

Prometheus слушает `127.0.0.1:19090` (`ACTIVITY_PROMETHEUS_PORT`). Цели внутри
сети Compose (без публикации наружу):

| Процесс | Management |
| --- | --- |
| api | `api:8082` (loopback `127.0.0.1:18082` уже в базовом compose) |
| worker | `worker:8081` |
| activity | `activity:8091` |
| crm-events | `crm-events:8092` |

Проверка:

```sh
wget -qO- http://127.0.0.1:19090/-/healthy
wget -qO- 'http://127.0.0.1:19090/api/v1/targets'
wget -qO- http://127.0.0.1:18082/metrics | head
```

Остановить overlay, не трогая данные пилота:

```sh
docker-compose -f docker-compose.activity.yml \
  -f docker-compose.activity-observability.yml rm -sf prometheus
```

## Импорт dashboard

1. Поднять любой Grafana (локально или существующий операторский).
2. Добавить Prometheus datasource на `http://127.0.0.1:19090` либо на свой scrape.
3. Dashboards → Import → загрузить `grafana/activity-pilot.json`.
4. Выбрать datasource в переменной `datasource`.

Dashboard **не** публикуется сам и не считается живым на целевом хосте.

## Правила и Alertmanager

`prometheus.yml` загружает `alerts.yml` и оставляет `alerting.alertmanagers`
пустым. Алерты видны в UI Prometheus (`/alerts`), но никуда не маршрутизируются.
Ставить Alertmanager на целевой сервер эта задача **не** делает.

`GatewayScrapeUnavailable` использует `up` и отсутствие цели `activity-worker`:
остановленный Gateway не способен обновить свой серверный RPC counter.
Если job переименован при импорте, соответствующий selector нужно изменить.
Агрегация очередей удаляет только `state`, сохраняя labels конкретной цели;
ошибки delivery сопоставляются с метрикой доступности без `code`.

```sh
make activity-observability-test
```

Эта проверка входит в `make activity-ci` и GitHub Activity gate. Она загружает
сам `alerts.yml`, проверяет реальные scrape labels, пустую очередь другой
цели, отсутствие Gateway, время `for` и выключение алертов после восстановления.

Пороги и SLO — в [ADR-0022](../../docs/adr/0022-activity-slo-and-observability.md)
и [runbook](../../docs/runbooks/activity-observability.md).

## Кардинальность labels

Разрешены только конечные классы: `state`, `outcome`, `code`, `method`,
`service`, `operation`, `route` (`panel`/`event`/`other`), `record`, `scope`.
ID установки, аккаунта, пользователя, пути и payload в labels не выводятся.
Детализация по аккаунту — защищённая диагностика виджета, не Prometheus.
