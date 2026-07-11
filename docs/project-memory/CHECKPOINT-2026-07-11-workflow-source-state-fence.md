# Checkpoint: lead-status workflow source-state fence

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/workflow-source-state-fence`

Base: `cd0d73e` (merged PR `#42`)

Implementation commit: `a097297`

Draft PR: https://github.com/sk1fy/amocrm-pro/pull/44

Canonical atomic Issue: https://github.com/sk1fy/amocrm-pro/issues/43

## Evidence and handoff

- normalized source pipeline/status копируется в immutable transition job рядом
  с target state;
- exact target завершается как already converged, exact source сохраняет
  compare/PATCH path, любое третье state завершается `source_changed` с
  `converged:false` без outbound effect/PATCH;
- incomplete/malformed source или target — permanent failure до amoCRM I/O;
- domain completion повторно проверяет job attempt, worker lease, typed ownership
  и active installation/integration в одной PostgreSQL transaction с run/audit;
- committed audit outcome служит bounded completed receipt: после crash между
  handler success и generic job completion reclaimed attempt возвращает тот же
  typed result без повторного GET/PATCH/effect, в том числе после tenant disable;
- три read-only subagent review нашли missing real retry/TLS evidence, stale
  completion race и crash-window receipt gaps; все P1 исправлены, final reviews
  не оставили P0/P1 findings.

## Validation actually run

Все Go/PostgreSQL команды выполнялись только через Docker/Make.

- `make fmt` — PASS.
- `make config` — PASS.
- `make openapi-check` — PASS.
- `make test` — PASS: runtime builds, formatting, vet и race-enabled full tests.
- `make integration-test` — PASS: five-migration PostgreSQL 17 cycle и все
  suites; новые workflow scenarios используют real TLS amoCRM client stub и
  покрывают source/target/third state, stale lease after GET, real attempt-2
  reclaim, disabled-tenant completed receipt и malformed payload matrix.
- `git diff --check` — PASS.

CI для final PR head проверяется отдельно в GitHub; этот checkpoint не выдаёт
наличие workflow за доказательство успешного remote run.

## Next

До merge канонический work item — https://github.com/sk1fy/amocrm-pro/issues/43.
После merge выполнить memory-sync: отметить acceptance и закрыть #43, затем
обновить parent #10 и aggregate #12. Самостоятельно PR не merge.

Checkpoint не содержит secrets, production payloads, raw amoCRM responses или PII.
