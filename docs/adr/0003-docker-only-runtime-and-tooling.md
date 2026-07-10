# ADR-0003: Docker-only runtime и tooling

- **Status:** Accepted.
- **Date:** 2026-07-10.
- **Decision owners:** project maintainers.

## Контекст

Основа должна воспроизводимо собираться и проверяться независимо от установленной на host версии Go/PostgreSQL. Она также станет частью микросервисной платформы, где runtime и CI поставляются как контейнеры.

Смешанный workflow (`go test` на host, PostgreSQL на host, сборка в Docker) создаёт разные toolchains, права на generated/cache files и расхождения между local и CI. Требование проекта: и Go, и базы данных выполнять через Docker.

## Решение

Все проектные Go-команды, сборка, тесты, форматирование, vet, миграции и PostgreSQL выполняются в Docker. Поддерживаемый developer interface — Makefile, оборачивающий Docker/Compose.

Зафиксированные baseline versions:

- Go 1.25 (`golang:1.25-alpine` по умолчанию);
- PostgreSQL 17 (`postgres:17-alpine`);
- отдельные Docker targets `api`, `worker`, `migrate`, `test`.

Host prerequisites ограничиваются Docker с Compose и `make`. Host Go, `psql` и локальный PostgreSQL не являются поддерживаемым execution path. Для интерактивного SQL используется `make db-shell`; для Go operations — соответствующие Make targets. CI также должен собирать/проверять Docker targets, а не зависеть от host Go setup.

Compose поднимает PostgreSQL, запускает migration container до API/worker и использует healthchecks/dependency conditions. Runtime containers запускаются с ограничениями read-only filesystem/no-new-privileges, где это предусмотрено конфигурацией.

## Последствия

### Положительные

- одинаковые Go/PostgreSQL versions локально и в CI;
- меньше скрытых host dependencies и проблем «работает у меня»;
- production-like startup ordering и migration path проверяются раньше;
- разработчику не нужно устанавливать Go/PostgreSQL;
- container build остаётся частью обязательной regression surface.

### Отрицательные и риски

- первый build зависит от Docker registry/network и занимает больше времени;
- cache/UID mapping отличаются между OS и требуют поддержки;
- Docker daemon становится обязательной developer dependency;
- Compose health не заменяет production orchestration/observability tests;
- platform differences Docker Desktop/Linux всё равно нужно учитывать.

## Поддерживаемые команды

Документация должна показывать project operations через Make/Docker, например:

```sh
make config
make build
make up
make test
make fmt-check
make migrate
make db-shell
make down
```

Прямые host-команды `go test`, `go build`, `gofmt` и `psql` не документируются как поддерживаемый workflow.

## Отклонённые варианты

### Host Go + контейнерный PostgreSQL

Отклонён из-за расхождения toolchain и CI/runtime images.

### Полностью host-local Go и PostgreSQL

Отклонён: противоречит deployment model и делает environment setup менее воспроизводимым.

### Devcontainer как единственный interface

Не выбран обязательным: он может быть добавлен поверх Docker workflow, но Make/Compose остаются portable baseline.

## Условия пересмотра

Разрешение альтернативного host workflow возможно только как необязательный convenience path, если он не меняет canonical versions и не заменяет Docker regression checks. Canonical build/release artifact остаётся Docker image.
