# Stage 8 independent review

Date: 2026-09-11. Reviewer did not commit, push, or reset.
Plan: `docs/plans/2026-09-08-activity-backend-development.md` § Этап 8 and Appendix G.

Mode: local tree for OPS-01/02, MOD-03, QA-01, tester 0.5.2, and coordinator docs.
Coordinator later ran `make ACTIVITY_TEST_PROJECT=amocrm-stage8-test activity-ci`
(exit 0) and addressed the issues below. `make integration-test` was not run.

## Summary

Local artefacts exist and several claimed invariants hold: new metric labels are finite, HTTP size does not put paths or bodies on series, backup/transfer scripts refuse `amocrm_*` and project `amocrm-activity`, alert *string* values match the code (`Unavailable` vs `unavailable`, cleanup `outcome="completed"`), widget traffic stays on the public Core origin, and the new deny/oversized unit tests pass without a nil-repository panic.

The stage is not clean. Two OPS-01 alerts cannot fire because of PromQL vector matching. Coverage gauges do not implement the product coverage rules in `periodState` / `bucketCoverage`. The documented observability `up -d` recreates the running `amocrm-activity` collector.

## Task verdicts

| Task | Verdict | Why |
| --- | --- | --- |
| OPS-01 | **fail** | Dashboard JSON and bounded series exist, but stuck-collection / outbox-growing alerts are dead PromQL, and coverage SQL disagrees with ACT-04 semantics. Target Grafana/Alertmanager correctly not claimed live. |
| OPS-02 | **pass (local)** | `verify-backup-owners.sh --isolated` is a synthetic three-owner restore with `_test` names and a hard refuse of runtime DBs. Target roles/KMS remain open. |
| MOD-03 | **conditional** | ADR-0023, overlay, certs usage text, and Events dump onto a second Postgres are in tree. Live Activity-then-Events process cutover and multi-host RTT are not done; the former is still marked `[x]`. |
| QA-01 | **pass (unit)** | Inventory plus compact Panel / EventCard / GetEvent deny and EventCard size mapping. Full Docker suite is a coordinator gate. |
| Tester | **pass (local)** | 0.5.2 source=vendor=AMD; Node checks cover Core origin and card allowlist. ZIP not rebuilt. Live install is QA-02. |
| QA-02 | **open** | Checklist only. Not an implementation bug. |
| OPS-03 | **open** | RC docs only. Pilot enable / target server not done. |

## Checks run here

- `gofmt -l` on the stage-8 Go files listed in OPS-01/QA-01: empty.
- `go test ./internal/services/crmevents` (`TestMetric*` + `TestUnauthorizedLocalCallsDoNotReachStorage`): PASS.
- `go test ./internal/activitybridge` (`TestUnavailableDeliveryMetrics*` + `TestHTTP*`): PASS.
- `go test ./internal/componentruntime` (`TestDatabaseSizeCollectorUnavailable*` + `TestLoadPreservesStaticAddressesAndRejectsForeignDSN`): PASS. `TestDatabaseSizeCollectorReportsOwnerBytes` not run (needs `TEST_DATABASE_URL`).
- `go test ./internal/services/activity` (`TestRevocationStopsLocalAdapterBeforeDataAccess` + `TestPublicReadsRejectExpiredWrongGrantAndUnavailablePolicy` + `TestEventCardMapsOversizedResponseLikeQuery`): PASS.

## Checked and not issues

- Cardinality of *new* series: `state`, `route=panel|event|other`, `service`. No installation/account/user/path/payload labels.
- `activity_http_response_bytes` is created once on the Bridge and gathered through `Collector()`; production does not `MustRegister` the HistogramVec a second time.
- `countingWriter` implements `Unwrap` and `Flush`.
- `service_rpc_requests_total` uses `status.Code(err).String()` (`Unavailable`); delivery errors use lowercase DB codes (`unavailable`); cleanup passes use `completed`.
- Backup/transfer scripts refuse `amocrm_core` / `amocrm_activity` / `amocrm_events` and compose project `amocrm-activity`; throwaway DBs end with `_test`. Transfer overlay overrides `name:` to `amocrm-activity-transfer-test`.
- Compact Panel, EventCard, and owner GetEvent deny paths run before storage; oversized EventCard shares `ValidateResponseSize` with Panel. Nil repository does not panic on those denies.
- Widget / tester: relative `/api/v1/widget/activity/...` plus `backend_url` Core origin; Node test forbids product hosts.
- QA-02, target Grafana/Alertmanager, physical multi-host RTT, and OPS-03 pilot enable stay `[ ]` or are explicitly “not on target”.

## Issues

### Issue 1 -- Severity: bug
- **File**: deploy/observability/alerts.yml:17
- **Description**: `CRMEventsStuckCollection` and `ActivityOutboxGrowing` add instant vectors that disagree on `state` (`queued` + `running` + …, `pending_delivery` + `delivering`). PromQL `+` matches on the full label set, so those sums are empty and the `and (… ) > 0` clause never holds. The stuck-collection / outbox-growing alerts cannot fire even when oldest-age gauges are high. `crm_events_jobs` also omits zero series for missing states, which would still need `sum()` / `or vector(0)` if the `+` were kept.
- **Suggestion**: Use `sum(crm_events_jobs{state=~"queued|running|retry|paused"}) > 0` and `sum(activity_delivery_commands{state=~"pending_delivery|delivering"}) > 0` (or `ignoring(state)`). Keep the age gauges as the stall bound.
- **Status**: fixed (coordinator: `sum(...)` over state regex; `+` on disagreeing labels removed)

### Issue 2 -- Severity: bug
- **File**: internal/services/crmevents/postgres_metrics.go:65
- **Description**: Product coverage is a selected interval vs `VerifiedFrom`/`VerifiedThrough`/`HistoryFrom`, where `Verified*` come from `event_sources.continuous_*` and `HistoryFrom = max(retained_from, verified_from)` (`postgres_read.go` `status`/`bucketCoverage`, `activity/service.go` `periodState`). The new gauge instead marks a source `verified` only when **one** `event_coverage` row satisfies `window_from <= COALESCE(retained_from, continuous_from)` and `window_to >= continuous_to`. After the first Retain tick, `retained_from` is `now - retention_days` (default 7) while `continuous_*` still covers `initial_days` (default 2). Prometheus then shows `partial` while a panel for the verified interval shows `verified`. `event_coverage` is also the wrong primitive: `cover()` already maintains `continuous_from`/`continuous_to` as the gap-free verified range.
- **Suggestion**: Classify like `periodState` for the retained/continuous interval: `unknown` when `continuous_from`/`continuous_to` are NULL; `verified` when that continuous range is set (optionally `partial` if `retained_from < continuous_from`, i.e. the retained window is wider than verified history). Do not require a single spanning `event_coverage` row. Add a Postgres test with `retention_days=7`, `continuous_*` of two days, and a retained frontier.
- **Status**: fixed (coordinator: classify from `continuous_*`/`retained_from` like retained-window periodState; no spanning `event_coverage` row)

### Issue 3 -- Severity: bug
- **File**: docker-compose.activity-observability.yml:2
- **Description**: Unused, the overlay does not edit `docker-compose.activity.yml`. Applied as documented (`docker-compose -f docker-compose.activity.yml -f docker-compose.activity-observability.yml up -d`) it keeps the base `name: amocrm-activity` and adds host ports to `worker`, `activity`, and `crm-events`. Those services currently publish no ports, so Compose recreates the running collector and product processes. Prometheus already scrapes `worker:8081` / `activity:8091` / `crm-events:8092` on the Docker network; the extra host ports are not required for scrape. The transfer overlay avoids this with `name: amocrm-activity-transfer-test` and an explicit “do not `up` onto `amocrm-activity`” warning. Observability README/`up -d` has no equivalent warning.
- **Suggestion**: Do not attach host ports to the three processes (exec wget / existing api management is enough), or override `name:` to a `*-test` project, and refuse/warn if the project is `amocrm-activity`. Document that applying the overlay to the live pilot recreates workers.
- **Status**: fixed (coordinator: overlay adds only Prometheus; no host ports on worker/activity/crm-events)

### Issue 4 -- Severity: suggestion
- **File**: internal/services/crmevents/postgres_metrics.go:96
- **Description**: `crm_events_coverage_gap_seconds` is `now() - coalesce(max(window_to), continuous_to, now())`, then outer `coalesce(..., 0)`. An enabled source with no coverage windows and NULL `continuous_to` (unknown / not yet collected) reports gap `0`, i.e. “caught up”, next to lag which is also 0 for the same NULL. The inner `now()` fallback makes that explicit.
- **Suggestion**: If there is no `window_to`/`continuous_to`, omit the gap or keep it NULL and do not publish a healthy zero. Align the dashboard so unknown sources cannot look current.
- **Status**: fixed (coordinator: gap published only when an enabled source has `continuous_to`)

### Issue 5 -- Severity: suggestion
- **File**: docs/plans/2026-09-08-activity-backend-development.md:830
- **Description**: MOD-03 item “На стенде с разными серверами/сетевыми адресами перенести Activity, затем независимо CRM Events…” is `[x]`. Evidence is Compose network config plus Events dump/restore onto a second Postgres. Activity was not moved, processes were not drained/cut over, and operation IDs/settings/grants were not checked on a live pair of addresses. The parenthetical admits this; the checkbox still reads done. QA-02, target Grafana, and RTT correctly remain open.
- **Suggestion**: Reopen that one MOD-03 line (or split “overlay config + Events data-plane restore” from “Activity then Events live cutover”). Do not treat Docker restore as physical transfer.
- **Status**: fixed (coordinator: checkbox reopened; overlay+Events restore remains documented, live cutover stays open)

### Issue 6 -- Severity: nit
- **File**: internal/activitybridge/http.go:186
- **Description**: `countingWriter.Flush` type-asserts only the immediate inner writer. AccessLog wraps with `statusWriter`, which is not a `Flusher` but does `Unwrap`. A handler that flushes through the counting writer therefore no-ops even when the net/http response supports Flush. Widget JSON does not stream, so this is latent.
- **Suggestion**: Walk `Unwrap()` until a `Flusher` is found, matching `http.NewResponseController`. Optional: `WriteHeader` passthrough is already provided by embedding.
- **Status**: fixed (coordinator: Flush walks `Unwrap()`)

## Residual open product work (not implementation bugs)

- QA-02 live amoCRM / SDK JWT/CORS/CSP / ZIP install.
- OPS-03 limited pilot enable and target-server deploy.
- Target Grafana and Alertmanager (rules are local files only).
- Physical multi-host RTT, deadline errors, connection budgets, throughput before/after (plan line 831, `[ ]`).
- Production backup roles, KMS/key access, host disk (`node_exporter` absent).
- Live backfill speed against amoCRM (ADR-0022 still open).
- Coordinator `make activity-ci` / `make integration-test` / `make test`.
