# Checkpoint: 100k expired-lease capacity evidence

Дата: 2026-07-12 (Europe/Moscow)

Ветка: `codex/reaper-100k-capacity`

Base: `7d495fe` (merged PR `#52`)

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/53

## Evidence and handoff

- PostgreSQL 17 integration bulk-seed создаёт 100,000 expired processing jobs
  без production payloads или PII;
- expired-lease selection проверяется через `EXPLAIN (ANALYZE, BUFFERS)` и
  использует существующий partial index `jobs_processing_lease_idx`;
- один claim с reaper batch `25` одновременно забирает higher-priority ready
  job, переводит ровно 25 expired jobs в retry, создаёт ровно 25
  `lease_expired` attempts и оставляет 99,975 expired processing jobs;
- двухминутный context deadline является только safety bound. Измеренное время
  seed, query plan и claim/reap записывается verbose integration output; tight
  wall-clock assertion отсутствует;
- production SQL, schema, config, public HTTP/OpenAPI contract и Redis не
  изменены: измерение не показало оснований для bulk-SQL redesign.

Финальное локальное измерение внутри race-enabled integration container:

- 100k bulk seed: `783.294114ms`;
- plan: `Index Scan using jobs_processing_lease_idx`, 33 index rows прочитано
  для result `LIMIT 25`, execution time `0.163ms`;
- full claim/reap transaction: `130.108625ms`.

## Validation actually run

Все Go/PostgreSQL операции выполнялись только через Docker/Make.

- `make fmt` — PASS;
- `make config` — PASS;
- `make openapi-check` — PASS;
- `make test` — PASS: formatting, vet и race-enabled full tests;
- `make integration-test` — PASS: guarded down, six-migration cycle,
  concurrent up и все PostgreSQL suites, включая 100k capacity scenario;
- `make build` — PASS: api, worker и migrate images;
- `git diff --check` — PASS.

Checkpoint не содержит credentials, production data, payloads или PII.
