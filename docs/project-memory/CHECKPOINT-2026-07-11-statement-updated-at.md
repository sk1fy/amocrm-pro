# Checkpoint: statement-time `updated_at` triggers

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/statement-updated-at`

Base: `a4bd4a4` (merged PR `#44`)

Implementation commit: `58715a5`

Draft PR: https://github.com/sk1fy/amocrm-pro/pull/46

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/45

## Evidence and handoff

- новая reversible migration `000006` заменяет transaction-time `now()` в
  общей `set_updated_at()` trigger-функции на `statement_timestamp()`;
- down migration восстанавливает прежнюю семантику `now()`;
- исторические migrations не изменены, поэтому их checksums сохранены;
- все существующие trigger sites продолжают использовать одну общую функцию;
- real PostgreSQL integration test держит одну транзакцию открытой, выполняет
  более поздний UPDATE и доказывает, что `updated_at` равен времени statement и
  позже `transaction_timestamp()`;
- Redis, runtime API, retention и migration CLI policy не изменялись.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: runtime builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: six-migration PostgreSQL 17 cycle
  `up -> down -> concurrent up`, checksum assertions и все integration suites,
  включая statement-time trigger test.
- `make build` — PASS: api, worker и migrate images.
- `git diff --check` — PASS.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/45.
После merge отметить acceptance и закрыть #45, затем обновить parent #21 и
aggregate #12. Самостоятельно PR не merge.

Checkpoint не содержит secrets, production payloads или PII.
