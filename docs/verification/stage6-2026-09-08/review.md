# Stage 6 independent review (REL-01, REL-02, REL-03, MOD-02)

Date: 2026-09-10. Reviewer did not commit and did not edit production code.
Plan: `docs/plans/2026-09-08-activity-backend-development.md` § Этап 6 and Appendix G.

## Verdict

| Task | Verdict | Why |
| --- | --- | --- |
| REL-01 | **pass** | Enrichment-aware collector cases exist, reuse owner helpers, and match current `Claim` / `Fail` / `ClaimEnrichment` / `Apply` semantics. No second collector. |
| REL-02 | **pass** | Thresholds were written before the run. L-01/L-02/L-03 were measured. No Redis, no read-path indexes, no aggregate tables. CI smoke does not `Skip` and does not write repo artifacts. |
| REL-03 | **pass** | Owner technical-history GC is a new bounded tick on top of unchanged 2..30 `crm_events` retention. 7-day floor, live-lease and non-terminal receipt keep, finite gauges, CORE-05 horizon documented not implemented. |
| MOD-02 | **conditional** | Embedded vs mTLS parity for Activity presentation/errors/Configure is real and passing. HTTP detail still fail-opens to the Events envelope when Activity is down. Process-cluster ping/lead-status/GET `/panel` was not re-run. Events Query/GetEvent parity is a fixture, not the owner SQL path. |

Coordinator ran `make ACTIVITY_TEST_PROJECT=amocrm-stage6-test activity-ci` after two test fixes ([fixes.md](fixes.md)): exit 0, 205 Go PASS / 3 helper SKIP / 0 FAIL, UI 25 PASS. REL-01/REL-03 owner tests and the REL-02 CI smoke executed against `events_components_test`. `TestComponentProcessesAndModeSwitch` and `TestComponentOSProcessFaults` passed in the same gate.

## Checks run here (no docker-compose / make activity-ci / make activity-test)

- `gofmt -l` on the eight stage-6 Go files: empty (formatted).
- `go test -count=1 -timeout 3m ./internal/servicerpc -run 'TestStage6|TestActivityBusinessLocalAndMTLSGRPCParity|TestStage2'`: PASS.
- `go test -c -o /tmp/crmevents-stage6.test ./internal/services/crmevents`: PASS (compile only).
- `go test -count=1 -timeout 1m ./internal/componentruntime -run TestDomainDependencyBoundaries`: PASS.
- `git diff`: `migrations/crmevents/000001`–`000006` untouched; `internal/componentruntime/process_integration_test.go` not rewritten.
- Non-test `internal/services/crmevents` files do not import `servicerpc` / `corepolicy` / `gateway` / `amocrm`.

## CI smoke (coordinator note)

`TestStage6ReadMeasure` no longer `Skip`s when `STAGE6_REL02_MEASURE` is unset. It calls `rel02CISmoke`, which has **no** `t.Skip`. The only Skip on that path is the package-standard `eventsPool` Skip when `CRM_EVENTS_TEST_DATABASE_URL` is unset — the same Skip every crmevents owner test uses, and activity-ci already sets the DSN.

Smoke does **not** call `rel02ArtifactDir` / `rel02WriteJSON` / `rel02WriteText`. It cannot dirty `docs/verification/stage6-2026-09-08/rel-02/` during CI. Artifact writes happen only when `STAGE6_REL02_MEASURE=true`.

## What landed vs the plan

**REL-01** is a check: production collector was not rewritten. New tests in `stage6_collector_integration_test.go` cover restart/crash-after-commit, fencing, page replay, late ID, unstable pagination with queued sidecar; 429/5xx/timeout/permission/reauth isolation (`Fail` vs `FailEnrichment`); enrichment not blocking or advancing `continuous_to` / coverage; current A before backfill B, enrichment refused under a live source lease; Configure conflict on a different payload, attach without rewriting the durable window, raising `retention_days` does not restore deleted rows.

**REL-02** is measurement plus a conditional “no change”. Thresholds in `rel-02.md` match the test constants. Profiles include small department, 100 employees, dense day, large notes, long backfill, 5-install L-01, 3 MiB L-03. EXPLAIN artifacts show `crm_events_time` / `crm_events_pkey` / `event_enrichment_claim` on the product page and card. Seq Scan on aggregates/prefix/entity/category at 35k rows is ≤20 ms from shared buffers; “изменение не требуется” is backed by those plans. First-run FAIL on a stricter Seq Scan rule is disclosed; thresholds were not raised.

**REL-03** adds `000007_technical_history`, splits `Retain` into `retainEvents` + one `HistoryBatch` under advisory lock `39081476393`, floors `HistoryHorizon` at 7d, keeps receipts while the operation is non-terminal, keeps a live matching source lease, moves counters onto `event_sources`, and excludes `completed`/`failed` from job gauges. ADR-0016, runbook, and Apply comment document post-GC replay as a new accept and the CORE-05 outbox floor.

**MOD-02** adds four focused tests in `stage6_parity_test.go` without copying `process_integration_test.go`. JSON meaning uses `UseNumber`; latency is not compared. Old Activity without GetEvent / presenter fail-closed. Configure replay after lost RPC and server restart is on the in-memory Activity inbox.

### Issues

### Issue 1 -- Severity: suggestion
- **File**: internal/servicerpc/stage6_parity_test.go:893
- **Description**: MOD-02 requires dependent Activity endpoints to return explicit unavailability. RPC `Panel` / `EventCard` and HTTP `GET /panel` do (503 / `unavailable`, no `events` array). Public `GET /api/v1/widget/activity/events/{id}` still returns **200** with the owner envelope and no `view` when Activity is down and Events is up. That is the pre-existing `activitybridge.GetEvent` fallback (`internal/activitybridge/bridge.go:74`), not a new stage-6 change. The test locks the fallback in rather than treating the URL as an Activity endpoint that must fail closed.
- **Suggestion**: Keep the fallback only if product wants a raw owner card without presentation while Activity is down. Otherwise return 503 for that URL too when `EventCard` is `unavailable`, and keep the owner fallback behind an explicit flag. Process-cluster GET `/panel` plus ping/lead-status on real binaries remains BLOCKED-FOR-COORDINATOR (`process_integration_test.go` already covers ping/lead-status; GET `/panel` does not).
- **Status**: open

### Issue 2 -- Severity: suggestion
- **File**: internal/servicerpc/stage6_parity_test.go:68
- **Description**: Local vs mTLS comparison for owner `Query` / `GetEvent` runs against `stage6Events`, an in-memory reimplementation of filters. Both sides share that fake, so the test proves protobuf/mTLS conversion and Activity presentation, not `postgres_read.go` filter/order/compact SQL. Durable Events `Apply` replay in the same file is also the fake inbox; Postgres lost-response replay stays in existing `TestCRMEventsOwnPostgresReplayAfterMTLSConnectionLostAfterCommit`, which this session did not run.
- **Suggestion**: Accept as transport parity (stage 2 already has Postgres compact/order and fail-closed new query fields), or add one owner GetEvent/card/categories case to the existing crmevents RPC Postgres suite instead of growing the fake.
- **Status**: open

### Issue 3 -- Severity: nit
- **File**: internal/services/crmevents/stage6_read_measure_integration_test.go:532
- **Description**: Query / GetEvent / Claim use 3 warmup + 25 samples and real P95/P99. Activity `Panel` and `EventCard` in `rel02MeasureSizes` are a **single** wall-clock sample compared to the P95 budget. The report’s 117 ms Panel figure is that one shot, not a percentile. It is conservative (one slow call fails) and the observed value is far under 1.2 s, so it does not green-wash a miss.
- **Suggestion**: Either sample Panel/EventCard like Query, or say “single-shot budget” in `rel-02.md` instead of P95/P99 for those two rows.
- **Status**: open

## Residual (not defects)

- REL-01 and REL-03 integration tests need `CRM_EVENTS_TEST_DATABASE_URL` on a `*_test` database. They compiled here; they were not executed.
- CORE-05 is intentionally not in this stage. ADR-0016 and the runbook state Core must not GC outbox/receipts earlier than 7 days after terminal. Down-migration `000007` does not restore deleted rows.
- New binary and `000007` must roll out together: `SavePage` / `MetricsSnapshot` now read `event_sources.events_*`.
- Technical-history GC of `paused` jobs after 7 days without a live matching lease includes disable/reauth paused cursors. A later `sync` creates a new job from `continuous_to`; that matches “identity is not eternal” in ADR-0016.
