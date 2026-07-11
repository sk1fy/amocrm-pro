# Checkpoint: async lead-status rule management

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/rule-management-contract`

Base: `2d21d88` (merged PR `#36`)

PR: https://github.com/sk1fy/amocrm-pro/pull/38

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/37

## Evidence and handoff

- verified widget user является durable management principal, но admin authority
  всегда re-checkится worker через live amoCRM user lookup;
- strict configure endpoint атомарно связывает disposable JWT,
  Idempotency-Key и typed job ownership;
- immutable source tuple и `expected_revision` задают create/update/disable CAS;
- rule mutation, audit и immutable per-job receipt коммитятся одной транзакцией;
- retry после применённой конфигурации возвращает receipt и не увеличивает
  revision повторно;
- actor-scoped job status возвращает только typed rule snapshot;
- migration `000005` добавляет revision и retained configuration receipts;
- ADR 0007 фиксирует principal, async authorization, CAS и receipt contract;
- `ROADMAP.md` теперь только recovery index с каноническим P0-P8 mapping;
- Issues #3-#13, #21 и #32 синхронизированы с merged evidence; #3, #5 и #7
  закрыты, остальные оставлены открытыми с явным remaining scope.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make test` — PASS: builds, formatting, vet, race-enabled tests.
- `make integration-test` — PASS: five-migration PostgreSQL 17
  `up -> down -> concurrent up`, strict HTTP admission, actor-scoped typed job
  result, create/update/disable, one-winner CAS race, receipt retry, non-admin
  and stale-lease rejection, enabled/disabled webhook routing, plus existing
  jobs/OAuth/webhook/widget/workflow suites.
- Финальный `make fmt`, `make config`, `make openapi-check`, `make test`,
  `make integration-test`, `git diff --check` — PASS.

## Next

После merge и закрытия #37 следующий work item выбирается из канонического
hardening backlog https://github.com/sk1fy/amocrm-pro/issues/21 либо создаётся
новый atomic Issue под оставшийся scope epic #10. Локального resume order нет.

Checkpoint не содержит secrets, JWTs, production payloads или PII.
