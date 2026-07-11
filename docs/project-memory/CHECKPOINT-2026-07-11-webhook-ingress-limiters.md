# Checkpoint: bounded configurable webhook ingress limiters

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/webhook-ingress-limiters`

Base: `c095479` (merged PR `#40`)

Implementation commit: `caaefc9`

PR: https://github.com/sk1fy/amocrm-pro/pull/42

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/41

## Evidence and handoff

- global и per-installation webhook token buckets настраиваются через validated
  API config; defaults сохраняют прежние `500/1000` и `20/40`;
- process-local semantics явны: distributed enforcement и Redis не добавлены;
- installation limiter registry использует один mutex для lookup/create,
  monotonic last-seen, token consumption и eviction, поэтому cleanup не создаёт
  второй bucket и не восстанавливает burst;
- lifecycle-owned cleanup удаляет idle entries вне request path; TTL default `1h`;
- management metrics имеют только фиксированные `scope`/`outcome` labels и
  показывают decisions, current entries и evictions без tenant/key labels;
- `429` сохраняет `Retry-After: 1`; rejected запросы не создают deliveries/jobs;
- global bucket намеренно остаётся до content-type/webhook-key lookup для защиты
  PostgreSQL, поэтому unknown keys тоже расходуют global capacity;
- три read-only subagent review подтвердили scope; один review нашёл возможный
  backward last-seen при scheduler reordering, исправленный до final gates.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: five-migration PostgreSQL 17 cycle и все
  suites, включая global/per-installation/unknown-key ingress scenarios,
  durable side-effect counts, metrics, eviction race и monotonic clock tests.
- `git diff --check` — PASS.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/41.
После merge выполнить memory-sync: отметить acceptance и закрыть #41, затем
обновить parent backlog #21 и aggregate status #12. Самостоятельно PR не merge.

Checkpoint не содержит secrets, production webhook keys/payloads или PII.
