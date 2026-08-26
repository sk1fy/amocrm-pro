# Checkpoint: isolated API management listener

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/management-listener`

Base: `f4b812d` (merged PR `#38`)

Implementation commit: `01f5736`

PR: https://github.com/sk1fy/amocrm-pro/pull/40

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/39

## Evidence and handoff

- публичный API listener сохраняет `/live` и business/webhook routes, но
  `/ready` и `/metrics` на нём возвращают `404`;
- отдельный configurable management listener публикует `/live`, PostgreSQL-backed
  `/ready` и Prometheus `/metrics`;
- public OpenAPI inventory больше не включает management-only routes;
- API использует отдельный Prometheus registry и единый supervisor двух
  listeners с coordinated graceful shutdown и error propagation;
- Compose публикует management port только на loopback и использует его для
  healthcheck;
- API image не содержит hard-coded healthcheck: это исключает ложный unhealthy
  при допустимом override `MANAGEMENT_HTTP_ADDRESS`; deployment/Compose должен
  задавать probe согласованно со своим listener address;
- PostgreSQL integration проверяет public/management isolation, ready `200`,
  unavailable `503`, metrics exposition и listener supervision;
- read-only subagent review нашёл hard-coded image healthcheck mismatch; он
  исправлен до финального validation pass.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: five-migration PostgreSQL 17 cycle и все
  integration suites, включая новый HTTP management route test.
- `git diff --check` — PASS.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/39.
После merge выполнить memory-sync: отметить acceptance и закрыть #39, затем
обновить parent backlog #21 и aggregate status #12. Самостоятельно PR не merge.

Checkpoint не содержит secrets, production payloads или PII.
