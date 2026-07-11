# Checkpoint: fail-closed destructive migration rollback

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/guard-migrate-down`

Base: `7a10a39` (merged PR `#46`)

Implementation commit: `b094670`

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/47

## Evidence and handoff

- migrate binary проверяет exact `MIGRATION_DOWN_CONFIRM` до загрузки database
  config и открытия PostgreSQL connection;
- `up` path не требует подтверждения и не изменён;
- Make target fail-closed и передаёт подтверждение только ephemeral migrate
  container, не сохраняя его в default Compose environment;
- isolated PostgreSQL gate сначала получает ожидаемый nonzero exit без
  подтверждения и доказывает сохранность всех шести migration records и
  application schema, затем выполняет confirmed down и concurrent up;
- runbook фиксирует destructive impact, target/backup prerequisites, точную
  Docker/Make invocation, verification и recovery;
- Redis, schema, runtime API и production data не затронуты.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: runtime builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: unconfirmed rollback refusal with intact
  PostgreSQL schema, confirmed `down`, six-migration concurrent `up` и все
  integration suites.
- `make build` — PASS: api, worker и migrate images.
- `git diff --check` — PASS.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/47.
После merge отметить acceptance и закрыть #47, затем обновить parent #21 и
aggregate #12. Самостоятельно PR не merge.

Checkpoint не содержит credentials, production data, payloads или PII.
