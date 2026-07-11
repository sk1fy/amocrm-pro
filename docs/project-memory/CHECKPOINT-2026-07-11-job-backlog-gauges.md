# Checkpoint: exact PostgreSQL job backlog gauges

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/job-backlog-gauges`

Base: `ca7f21e` (merged PR `#48`)

Implementation commit: `a44efe1`

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/49

## Evidence and handoff

- worker registry публикует `amocrm_jobs_backlog{kind}` только с тремя
  фиксированными значениями `ready`, `scheduled`, `expired_lease`;
- один read-only PostgreSQL statement на scrape считает все значения в одном
  snapshot относительно `statement_timestamp()`;
- ready/scheduled используют queue status, attempts и run-after eligibility;
  expired lease включает только processing rows с прошедшим `locked_until`;
- collector использует existing database timeout; query failure возвращает
  invalid Prometheus metric и не публикует stale/fabricated values;
- real PostgreSQL integration покрывает ready, retry, future, exhausted,
  live/expired leases, terminal rows, live mutation и explicit zero series;
- labels не содержат tenant, job type, resource, error или payload;
- Redis, schema, API, dashboard/alert policy и load generation не добавлены.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: runtime builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS после исправления только multi-statement test
  fixture: rollback guard, six-migration cycle и все PostgreSQL suites, включая
  exact backlog collector scenarios.
- `make build` — PASS: api, worker и migrate images.
- `git diff --check` — PASS.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/49.
После merge отметить acceptance и закрыть #49, затем обновить parent #21 и
aggregate #12. Самостоятельно PR не merge.

Checkpoint не содержит credentials, production data, payloads или PII.
