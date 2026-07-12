# Checkpoint: independently bounded expired-lease reaping

Дата: 2026-07-12 (Europe/Moscow)

Ветка: `codex/reaper-batch-limit`

Base: `6abc4f2` (merged PR `#50`)

Implementation commit: `9eb8ddc`

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/51

## Evidence and handoff

- worker принимает независимый `WORKER_REAP_BATCH_SIZE` с default `100` и
  допустимым диапазоном `1..1000`;
- expired и exhausted maintenance work в одной claim-транзакции ограничен этим
  значением и больше не вычисляется из размера ready-job claim;
- существующие retry/dead transitions, attempt records, failure observers,
  `SKIP LOCKED` и attempt fencing сохранены;
- PostgreSQL integration bulk-seed создаёт 1000 expired processing jobs и одну
  ready job; при reaper batch `25` ровно 25 leases переходят в retry, создаются
  ровно 25 `lease_expired` attempts, 975 expired leases остаются processing, а
  ready job одновременно успешно claim'ится;
- schema, public HTTP contract и Redis не добавлены и не изменены.

## Validation actually run

Все Go/PostgreSQL операции выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: guarded down, six-migration cycle,
  concurrent up и все PostgreSQL suites, включая 1000-row reaper scenario.
- `make build` — PASS: api, worker и migrate images.
- `git diff --check` — PASS.

## Next

Канонический work item: https://github.com/sk1fy/amocrm-pro/issues/51.
После открытия draft PR дождаться green CI. PR самостоятельно не merge.

Checkpoint не содержит credentials, production data, payloads или PII.
