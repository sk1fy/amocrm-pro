# ADR-0002: Два Go runtime-бинарника в одном модуле

- **Status:** Accepted.
- **Date:** 2026-07-10.
- **Decision owners:** project maintainers.

## Контекст

Публичный HTTP ingress и фоновые интеграционные операции имеют разные профили нагрузки и отказов. Webhook handler должен быстро сохранять delivery и отвечать, тогда как OAuth refresh, amoCRM API calls, parsing, sync и workflows могут быть долгими, повторяемыми и ограниченными внешним rate limit.

При этом на этапе основы оба процесса используют один домен, одну SQL schema и общие platform packages. Разделение на независимые Go modules/repositories сейчас добавило бы versioning и release coordination без доказанной выгоды.

## Решение

Поддерживать два независимо запускаемых долгоживущих Go-бинарника в одном module `github.com/sk1fy/amocrm-pro`:

- `cmd/api` — публичный `amocrm-api`;
- `cmd/worker` — внутренний `amocrm-worker`.

`cmd/migrate` является отдельной короткоживущей operational utility, а не третьим runtime-сервисом бизнес-архитектуры.

Каждый runtime получает собственный Docker target, конфигурацию, имя сервиса, lifecycle и health endpoint. Общий код остаётся под `internal/`; packages не должны зависеть от `cmd/*`. API не выполняет длительные amoCRM/workflow операции синхронно: оно сохраняет durable state/job, worker забирает работу из PostgreSQL.

Бинарники могут масштабироваться и выкатываться как отдельные deployment units, но на текущем этапе собираются из одной версии исходников и разделяют одну migration/schema contract.

## Последствия

### Положительные

- HTTP ingress изолирован от долгих jobs и внешних API failures;
- API и worker можно независимо настраивать и масштабировать;
- общие модели/config/logging/PostgreSQL packages не дублируются;
- один module упрощает atomic refactoring и согласование schema contracts;
- единый Docker test target проверяет совместимость общего кода.

### Отрицательные и риски

- изменение общего package может затронуть оба runtime одновременно;
- один repository/release требует дисциплины обратной совместимости при rolling deploy;
- границы пакетов легче случайно размыть, чем между отдельными repositories;
- worker всё равно имеет HTTP health server, что нужно учитывать в deployment/security policy.

Для контроля нужны явные package boundaries, backward-compatible migrations, раздельные image targets и contract tests для хранимых job payloads/normalized events.

## Отклонённые варианты

### Один combined process API + worker

Отклонён: разные failure/load profiles нельзя независимо масштабировать, а долгие jobs повышают blast radius ingress.

### Отдельный Go module/repository для каждого сервиса

Отложен до появления независимых ownership/release cadence или устойчивой межсервисной API boundary. Сейчас это усложнит совместные schema/domain changes.

### Отдельный сервис-мигратор как постоянный runtime

Отклонён: мигратор должен завершаться после применения migrations и использоваться как deployment init job.

## Условия пересмотра

Решение пересматривается, если API и worker получают разных владельцев/release cycles, общий module становится источником блокирующих связей либо worker разделяется на домены с самостоятельными SLO. Разделение требует зафиксированных contracts для jobs/events/API и migration ownership.
