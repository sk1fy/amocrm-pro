# OPS-03. Приёмка и развёртывание — release candidate

Дата: 11 сентября 2026. Это **кандидат**, не факт выпуска. Pilot enable,
дефекты живого виджета и выкладка на целевой сервер в этой сессии не
выполнялись.

## Состав кандидата

| Поле | Значение |
| --- | --- |
| Исходный commit | `d7cb8c7a8b988df8401765581eb8fd7bef86f7db` |
| Дерево | грязное: этап 8 поверх main |
| Тестер ZIP | 0.5.2, SHA-256 `da6072eb003afa8ee1889c9e62b75cac358810f7288f7267d5fb00da87f884b7` |
| Compose runtime | `amocrm-activity` не обновлялся |

### Изменения относительно `d7cb8c7`

- Bounded-метрики: coverage/gap CRM Events, размер widget HTTP, размер owner DB.
- Importable Grafana dashboard и Prometheus rules (`deploy/observability/`).
- Optional overlay scrape: `docker-compose.activity-observability.yml`.
- Synthetic restore всех owner DB и transfer overlay/скрипт.
- Unit-тесты публичных compact/EventCard/GetEvent deny и oversized mapping.
- Документы ADR-0022/0023 и runbooks наблюдаемости, backup, переноса.

Бизнес-логика сборщика, карточек и публичных URL панели не менялась.

### Миграции, которые должен получить runtime до нового бинаря

Порядок и rollback: [migrations-compat.md](migrations-compat.md). На runtime
этапа 1 были только Core ≤000011, Activity 000001, Events 000001.

1. CRM Events: `000002` … `000008` согласованно с бинарём CRM Events.
2. Core: `000012`, `000014`, `000015` согласованно с API/worker.
3. Activity owner: новых миграций в этапах 2–8 нет.

Down не возвращает удалённые строки, ciphertext и tombstones. Старые cursors
после EVT-02 нужно начать заново.

### Конфигурация пилота

Не менять сохранённые `initial_days`/`retention_days`. SLO и объём:
[ADR-0022](../../adr/0022-activity-slo-and-observability.md). Один Gateway.
Публичный origin виджета — Core HTTPS; перенос Activity/Events его не меняет.

### Rollback

1. Вернуть предыдущие образы API/worker/Activity/CRM Events.
2. Не выполнять down, если цель — сохранить данные.
3. Restore — из последних owner dump по [activity-backup-restore.md](../../runbooks/activity-backup-restore.md).
4. Compute-only откат — прежние статические адреса и тот же DSN.
5. ZIP 0.5.2 можно оставить: клиентский адаптер в этой сессии не менялся.

### Результаты тестов (локально, не GitHub CI)

Записаны субагентами: unit-метрики, QA-01 unit, backup-owners, transfer script,
tester 45 PASS. Координатор: `make ACTIVITY_TEST_PROJECT=amocrm-stage8-test activity-ci`
exit 0 (228 Go PASS / 3 helper SKIP / UI 25 PASS) **до** правок ревью (PromQL,
coverage class, overlay). Повтор targeted unit после правок — PASS.
`make integration-test` и `make test` не запускались.

### Pilot enable

Не выполнялся. Когда будет доступ:

1. Применить миграции и образы на выбранной installation.
2. `activity-control` / capability `activity` только для пилотного аккаунта.
3. Загрузить ZIP 0.5.2, пройти [qa-02.md](qa-02.md).
4. Смотреть lag/coverage/outbox/reauth по dashboard; не включать второй Gateway.

## Issues

GitHub #11/#21: артефакты dashboards/SLO/backup локальные; production-пункты
остаются открытыми. Этот файл не закрывает epic P8.
