# Observability server deployment (amocrm-pro)

Постоянное серверное развёртывание мониторинга поверх существующего стека
`amocrm-pro`. Это отдельный Compose-проект `observability`, который **не**
трогает контейнеры приложения и не перезапускает production-стек.

Отдельный от [локального пилота](../../README.md): у пилота Prometheus в
`tmpfs`, retention `6h`, нет Grafana/Loki/Alertmanager. Здесь — постоянные
тома, retention, дашборды и доставка уведомлений.

## Топология

```text
Метрики приложений ──→ Prometheus ──→ Alertmanager ──→ webhook-sink (тест)
Метрики сервера ──────→     │
                           └────────→ Grafana  (https://…/grafana/)
                                         ↑
Docker-логи ──→ Alloy ──→ Loki ───────────┘
```

| Компонент | Версия | Digest |
| --- | --- | --- |
| Prometheus | v2.55.1 | `sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3` |
| Alertmanager | v0.28.0 | `sha256:d5155cfac40a6d9250ffc97c19db2c5e190c7bc57c6b67125c94903358f8c7d8` |
| Node Exporter | v1.8.2 | `sha256:4032c6d5bfd752342c3e631c2f1de93ba6b86c41db6b167b9a35372c139e7706` |
| Loki | 3.3.2 | `sha256:8af2de1abbdd7aa92b27c9bcc96f0f4140c9096b507c77921ffddf1c6ad6c48f` |
| Grafana Alloy | v1.5.1 | `sha256:01a63f4e032ce54ee94b22049bc27f597e74f85566478c377f4b5c7f020c1eb3` |
| Grafana | 11.3.1 | `sha256:fa801ab6e1ae035135309580891e09f7eb94d1abdbd2106bdc288030b028158c` |
| webhook-sink (тест) | python:3.12-alpine | `sha256:b64631e04e4920160c50fbe8d8df828f7f35f06f425cb44aa09bca53e708a35a` |

Все образы закреплены по digest в [docker-compose.yml](docker-compose.yml).

## Расположение на сервере

- **Конфигурации (в Git)**: `/root/amocrm-pro/deploy/observability/server/`
  (этот каталог). Правила приложений — в соседнем `../alerts.yml`.
- **Данные (bind-mount)**: `/opt/amocrm-observability/data/{prometheus,alertmanager,loki,alloy,grafana}`.
- **Секреты**: `/opt/amocrm-observability/secrets/` (не в Git):
  - `grafana_admin_password` — пароль администратора Grafana (uid 472).
  - `grafana_metrics_password` — basic-auth для `/grafana/metrics` (uid 472 и 65534).
  - `alertmanager_webhook_url` — URL получателя уведомлений (сейчас — тестовый sink).

Секреты читаются контейнерами по UID, поэтому файлы `0644`. Хост однопользовательский
(только root), каталог `/opt` доступен только локально.

## Сети и доступ

- Сеть `observability_monitoring` — внутренняя bridge для компонентов мониторинга.
- Сеть `amocrm-pro_backend` (external) — Prometheus подключён к ней, чтобы скрейпить
  `api:8082`, `worker:8081`, `activity:8091`, `crm-events:8092` по Docker DNS **без**
  публикации дополнительных host-портов у приложений.
- Наружу опубликован только Grafana: `127.0.0.1:3000`, проксируется nginx на
  `https://2-26-66-187.sslip.io/grafana/` (location в `/etc/nginx/sites-available/amocrm`).
- Prometheus, Loki, Alertmanager, Alloy, Node Exporter и webhook-sink **не** опубликованы
  на хост и недоступны из интернета.

Проксирование Grafana в nginx (файл `/etc/nginx/sites-available/amocrm`, не в Git —
ниже воспроизводимый фрагмент). Важно: `proxy_pass` **без** завершающего слэша,
потому что Grafana запущена с `GF_SERVER_SERVE_FROM_SUB_PATH=true` и ждёт полный путь
`/grafana/...`; со слэшем получается бесконечный 301-редирект на самого себя.

```nginx
location /grafana/ {
    proxy_pass http://127.0.0.1:3000;   # НЕ "...3000/"
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 120s;
    proxy_send_timeout 120s;
}
```

Grafana: персональный вход, анонимный доступ и самостоятельная регистрация выключены
(`GF_AUTH_ANONYMOUS_ENABLED=false`, `GF_USERS_ALLOW_SIGN_UP=false`). URL и логин:

- URL: `https://2-26-66-187.sslip.io/grafana/`
- Логин: `admin`
- Пароль: файл `/opt/amocrm-observability/secrets/grafana_admin_password` на сервере.

## Targets и источники логов

Scrape (job → Docker DNS):

| Job | Target | Что собирает |
| --- | --- | --- |
| `activity-api` | `api:8082` | Core API /metrics (outbox, limiter, component_db) |
| `activity-worker` | `worker:8081` | Core worker /metrics (jobs, cleanup, RPC, amoCRM v4) |
| `activity` | `activity:8091` | Activity /metrics (component_db, RPC) |
| `crm-events` | `crm-events:8092` | CRM Events /metrics (coverage, enrichment, jobs) |
| `node-exporter` | `node-exporter:9100` | CPU/RAM/диск/inode/сеть хоста |
| `monitoring` | prometheus/alertmanager/loki/alloy | Самомониторинг |
| `grafana` | `grafana:3000/grafana/metrics` | Метрики Grafana (basic auth) |

Логи (Alloy → Loki): контейнеры проектов `amocrm-pro` и `observability`. JSON-логи
api/worker разбираются (time/level/msg), `request_id`/`method`/`route`/`status` — в
structured metadata (не индексируемые labels). Текстовые логи activity/crm-events
разбираются regex. Индексируемые labels: `environment`, `host`, `service`
(+ встроенные `container_name`, `compose_project`, `service_name` и `level`).

## Retention и лимиты

| Данные | Срок/лимит |
| --- | --- |
| Метрики Prometheus | 15 дней (`--storage.tsdb.retention.time=15d`) + потолок 8 GiB (`retention.size`) |
| Логи Loki | 7 дней (`limits_config.retention_period=168h`, удаление — Compactor) |
| Grafana/конфиги | постоянно в Git; данные Grafana — bind-mount + backup |
| Docker-логи приложений | см. «Известные ограничения» (ротация не настроена) |

Лимиты ресурсов (cpus/mem_limit) заданы в compose: prometheus/loki/grafana `0.5`/`512m`,
alloy `0.25`/`256m`, alertmanager `0.1`/`128m`, node-exporter `0.05`/`64m`. Суммарно
~2 GiB, запас для PostgreSQL и workers сохраняется.

## Дашборды и алерты

Дашборды (provisioning, постоянные UID, папка `Observability`):

| UID | Название |
| --- | --- |
| `platform-overview` | Обзор платформы (доступность, ошибки, очереди, задержки) |
| `system-status` | Статус системы (текущие цели, алерты, admin → Core, ресурсы) |
| `activity-events` | Activity / CRM Events (lag, coverage, outbox, enrichment, RPC, cleanup) |
| `server` | Сервер (CPU, RAM, диск, inode, сеть) |
| `monitoring` | Мониторинг (Prometheus, Loki, Alloy, доставка уведомлений) |
| `logs` | Логи (поиск по сервису, уровню, содержимому) |

Карточки `Targets up` / `Targets down` показывают instant-снимок на конец
выбранного периода, при диапазоне до `now` — последнее состояние scrape.
`sum(up == bool 1)` / `sum(up == bool 0)` возвращают ноль, когда цели есть,
но ни одна не соответствует условию. Если ряды `up` отсутствуют или устарели,
карточки показывают серое «Нет данных», а не зелёный ноль. Прошлые сбои остаются
на графике `Scrape availability`; они не должны сохраняться в текущих карточках
после восстановления. Аналогичная карточка на `system-status` также instant.

Правила: приложения — `../alerts.yml` (общие с пилотом, проверяются `promtool test rules`),
сервер/мониторинг — `prometheus/server-alerts.yml`. Единая цепочка
Prometheus rules → Alertmanager → получатель; Grafana Alerting не используется.

## Обслуживание

```bash
cd /root/amocrm-pro/deploy/observability/server

# статус
docker compose -f docker-compose.yml ps

# старт/стоп (только проект мониторинга; приложение не трогается)
docker compose -f docker-compose.yml up -d
docker compose -f docker-compose.yml stop        # или down (без -v: тома сохраняются)

# перечитать конфиг Prometheus без рестарта
docker exec observability-prometheus-1 wget -qO- --post-data '' \
  http://127.0.0.1:9090/-/reload

# перечитать Alertmanager
docker exec observability-alertmanager-1 wget -qO- --post-data '' \
  http://127.0.0.1:9093/-/reload

# логи
docker compose -f docker-compose.yml logs -f <service>
```

## Проверка конфигураций (версии соответствуют образам)

```bash
cd /root/amocrm-pro/deploy/observability
# Prometheus config + rules + поведенческие тесты
docker run --rm --entrypoint /bin/promtool \
  -v "$PWD/server/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
  -v "$PWD/alerts.yml:/etc/prometheus/alerts.yml:ro" \
  -v "$PWD/server/prometheus/server-alerts.yml:/etc/prometheus/server-alerts.yml:ro" \
  prom/prometheus:v2.55.1 check config /etc/prometheus/prometheus.yml
docker run --rm --entrypoint /bin/promtool -v "$PWD:/config:ro" \
  prom/prometheus:v2.55.1 test rules /config/alerts.test.yml

cd server
# Alertmanager
docker run --rm --entrypoint /bin/amtool \
  -v "$PWD/alertmanager/alertmanager.yml:/config.yml:ro" \
  -v /opt/amocrm-observability/secrets/alertmanager_webhook_url:/etc/alertmanager/webhook-url:ro \
  prom/alertmanager:v0.28.0 check-config /config.yml
# Alloy (формат/синтаксис)
docker run --rm -v "$PWD/alloy/config.alloy:/etc/alloy/config.alloy:ro" \
  grafana/alloy:v1.5.1 fmt -t /etc/alloy/config.alloy
# Compose
docker compose -f docker-compose.yml config --quiet
```

## Проверка и обновление только дашбордов

Из корня репозитория:

```bash
make server-dashboard-test
```

Проверка работает в Docker: валидирует instant-режим карточек и запускает
`promtool` на выражениях, извлечённых непосредственно из JSON. Сценарии:
все цели доступны, часть/все недоступны, восстановление, stale-маркеры,
устаревшие samples и полное отсутствие рядов. Она также входит в
`make activity-observability-test` и существующую Activity CI-проверку.

Для обновления только JSON доставьте проверенные файлы в каталог
`/root/amocrm-pro/deploy/observability/server/grafana/dashboards/` на сервере.
Он целиком примонтирован в `/etc/grafana/dashboards`; Grafana перечитывает
provisioning каждые 30 секунд. Перезапуск Grafana, Prometheus и приложений
не нужен. После одного интервала обновите страницу дашборда и проверьте:

1. На `platform-overview` карточки совпадают с instant-запросами Prometheus
   `sum(up == bool 1)` / `sum(up == bool 0)`; при всех доступных целях down = 0.
2. `system-status` показывает то же число недоступных целей.
3. При выборе периода, включающего прошлый сбой и восстановление, карточки
   показывают состояние на конец периода, а график сохраняет историю сбоя.
4. В Query inspector запросы карточек имеют `instant: true`, `range: false`.

Не останавливайте реальные сервисы ради проверки отказа: эти сценарии покрыты
`promtool`. Откат JSON выполняется восстановлением предыдущих версий тех же
файлов и ожиданием следующего цикла provisioning. Изменения через UI Grafana
могут быть перезаписаны provisioning; эталонные дашборды хранятся в Git.

## Обновление и откат

Обновление: поменять digest/версию в `docker-compose.yml` (и при необходимости
конфиги), прогнать проверки выше, затем:

```bash
docker compose -f docker-compose.yml up -d
```

Откат: вернуть прежние digest/версии и конфиги (они в Git), затем `up -d`.
**Не удалять** `/opt/amocrm-observability/data` при обычном откате. Откат образа не
всегда означает откат формата хранилища: если новая версия Loki/Prometheus успела
записать данные в новом формате, старый образ может их не прочитать. Перед крупным
обновлением — резервная копия данных (см. ниже). PostgreSQL и данные бекенда не
затрагиваются.

## Backup и восстановление

Обязательно в Git (уже): compose, конфиги, provisioning, дашборды, этот README.

Дополнительно периодически:

```bash
# конфиги + данные Grafana (SQLite) и секреты
tar -C /root/amocrm-pro/deploy/observability -czf /root/backups/observability-config-$(date +%F).tgz server
tar -C /opt/amocrm-observability/data -czf /root/backups/grafana-data-$(date +%F).tgz grafana
tar -C /opt/amocrm-observability -czf /root/backups/observability-secrets-$(date +%F).tgz secrets
```

Проверка восстановления: поднять стек в изолированном окружении (другая машина или
`docker compose -p obs-restore ...`) из Git + копии `grafana/` и `secrets/`, убедиться,
что Grafana стартует и provisioning поднимает источники/дашборды.

История Prometheus/Loki: допустимая потеря и способ резервного копирования — решение
принимается отдельно. Копирование «на лету» активных файлов TSDB/Loki не гарантирует
согласованную копию; для согласованного снимка используйте штатный снапшот:
`POST /api/v1/admin/tsdb/snapshot` (Prometheus, временно) или полную остановку на время
копирования. Для данного объёма (см. ниже) приемлема стратегия «конфиги воспроизводимы,
метрики/логи — пересоздаются», потерю истории считаем допустимой.

## Известные ограничения и открытые вопросы

1. **Канал уведомлений** — сейчас Alertmanager шлёт в локальный тестовый
   `webhook-sink` (`docker logs observability-webhook-sink-1`). Доставка проверена на
   тестовом алерте (firing + resolved). Реальный канал (email/Slack/…) нужно указать;
   для подключения достаточно записать URL в
   `/opt/amocrm-observability/secrets/alertmanager_webhook_url` и
   `POST http://…:9093/-/reload`.
2. **APP_ENV=development** — приложение в `.env` запущено с `APP_ENV=development`,
   поэтому его собственные логи содержат `"environment":"development"`, тогда как
   observability метит хост как `environment=production`. Стоит уточнить, какое
   значение должно быть эталонным.
3. **Docker socket у Alloy** — Alloy монтирует `/var/run/docker.sock:ro` для чтения
   логов. `:ro` не делает Docker API read-only. Alloy считается доверенным компонентом
   этого хоста. При необходимости ужесточения — socket-proxy (например,
   `tecnativa/docker-socket-proxy`) перед Alloy.
4. **Ротация Docker-логов** — daemon `json-file` без ротации; лимиты размера/числа
   локальных логов не заданы. Установка `max-size`/`max-file` в `daemon.json` или
   per-container `logging` требует пересоздания контейнеров — вынесено в отдельный
   запланированный шаг (не выполнено, чтобы не пересоздавать приложения).
5. **Loki delete-request store** — `delete_request_store: filesystem` настроен, но API
   `DELETE /loki/api/v1/delete` с filesystem-бекендом в single-binary не используется.
   Retention по возрасту выполняет сам Compactor. Явное удаление по запросу — вне
   текущих задач.
6. **`service_name`** — встроенный label `loki.source.docker` (дублирует `service`),
   не удаляется через relabel. Кардинальность ограничена (имя сервиса Compose).
7. **Внешний наблюдатель** — весь мониторинг на одном хосте с приложением; локальный
   Alertmanager не уведомит при полном отказе хоста. Нужен внешний uptime-контроль
   (не настроен).

## Приёмка (что проверено)

- Все 10 scrape targets `up=1` (api/worker/activity/crm-events/node-exporter/grafana/prometheus/alertmanager/loki/alloy).
- Логи приложений и мониторинга поступают в Loki; JSON разбирается, `request_id` в
  structured metadata, время записей корректное.
- Данные переживают рестарт Prometheus и Loki (история доступна после рестарта).
- Рестарт Alloy не вызывает массового повторного импорта (позиции в
  `/opt/amocrm-observability/data/alloy`).
- Тестовый алерт (реальный `ScrapeTargetDown` из-за кратковременного падения Grafana/Loki
  при настройке) доставлен в sink как `firing`, а после восстановления — как `resolved`.
- `promtool check config`, `promtool test rules`, `amtool check-config`, `alloy fmt -t`,
  `docker compose config` — проходят.
- Бекенд amocrm-pro работает без нарушений (все контейнеры healthy, API отвечает).
