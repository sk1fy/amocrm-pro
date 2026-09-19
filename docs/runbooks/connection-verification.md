# Актуальность подключения

Источник: [ADR-0031](../adr/0031-admin-connection-verification.md).

## Настройки Worker

| Env | По умолчанию | Значение |
| --- | --- | --- |
| ADMIN_CONNECTION_CHECKS_ENABLED | false | Включить расписание |
| ADMIN_CONNECTION_CHECK_INTERVAL | 1h | 1m–72m, jitter ±20% |
| ADMIN_CONNECTION_CHECK_TICK | 30s | 1s–1m |
| ADMIN_CONNECTION_CHECK_CONCURRENCY | 4 | Глобально 1–32 |
| ADMIN_CONNECTION_CHECK_INTEGRATION | пусто | UUID пилотной интеграции |
| ADMIN_CONNECTION_CHECK_ACCOUNT | пусто | ID пилотного аккаунта |
| ADMIN_CONNECTION_CHECK_PERCENT | 100 | Детерминированный охват 0–100 |

Предел по интеграции — 2, по установке — 1. Все реплики используют одинаковый
глобальный предел. Учесть дополнительные PostgreSQL sessions: до глобального
предела checks сверх рабочих pool, плюс scheduler session на реплику.
Срок актуальности — 5400 секунд (90 минут), единственная константа backend
`connectioncheck.FreshFor`; UI получает его в API и не пересчитывает свежесть.

## Выпуск

1. Выпустить polling Admin UI со старым Core.
2. Применить миграцию Core, выпустить API/Worker и совместимый Admin adapter.
3. Проверить ручной check: терминальная квитанция, свежий snapshot, интерфейс.
4. Включить scheduler только для тестовой интеграции или аккаунта.
5. Наблюдать не менее 24 часов rate limit, latency, auth errors, backlog.
6. Убрать pilot scope и последовательно установить процент 5, 25, 100.
7. При отклонениях выключить `ADMIN_CONNECTION_CHECKS_ENABLED`.

Выключение останавливает новые назначения; уже принятые jobs завершаются.
Оно не отключает UI polling и ручной check. Нормальная проверка повторяется через 48–72 минуты (целевой час с jitter).
После сетевых/внутренних ошибок backoff растёт до шести часов; Retry-After
при 429 соблюдается даже если он больше этого предела.
Начальная выборка рассредоточена
по целевому интервалу; seed — максимум 100 установок за tick. После сбоев
повтор может быть позже срока свежести; тогда интерфейс показывает stale.
Новые OAuth credentials скрывают старое наблюдение. `auth_error` не является
доказательством удаления виджета из amoCRM.

## Наблюдаемость

[Dashboard](../../deploy/observability/grafana/connection-checks.json) и
[правила](../../deploy/observability/connection-checks.yml) предназначены для
локальной проверки и импорта в целевой стек. Population gauges — снимок всей
БД на каждой реплике: в Grafana применять `max`, не `sum`. Счётчики попыток и
inflight локальны процессу: суммировать. Labels содержат только конечные
classification/state, без tenant ID, доменов, email и секретов.

При частичном pilot охвате population отражает всех active: warning о доле
непроверенных интерпретировать с учётом rollout. Critical требует устойчивого
массового технического отказа, единичный auth_error его не вызывает.
Production rollout и 24-часовое наблюдение не выполнены в этой локальной работе.

Worker compose (`docker-compose.yml` и `docker-compose.activity.yml`) передаёт
все указанные env. После изменения пересоздать Worker штатным deployment;
действующий процесс не перечитывает env. Не перезапускать пилотный стек
автоматически при локальной проверке реализации.
