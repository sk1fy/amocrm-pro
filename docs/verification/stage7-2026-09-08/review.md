# Stage 7 independent review (CORE-01..CORE-06)

Date: 2026-09-10. Reviewer did not commit and did not edit production/source files.
Plan: `docs/plans/2026-09-08-activity-backend-development.md` § Этап 7 and Appendix G.
ADRs read: 0016, 0017, 0018, 0019, 0021.

Mode: local uncommitted tree (staged + unstaged + untracked) for stage 7.

## Summary

CORE-01..05 land the claimed platform gaps: OAuth refresh no longer holds a SQL transaction across amoCRM HTTP, installation uninstall unregisters webhooks without purging owner data, OAuth start/callback have a process limiter and a non-enumerating JSON envelope, enrichment stays on the existing process-local v4 budget, and Core plus CRM Events now share a 7-day calendar redelivery horizon with tombstones. CORE-06 is locally complete (startup validation, secrets-rotation runbook, ADR-0021) but production SSH remains open, which is the task’s own residual. CORE-07 was not run; existing lead-status/webhook/widget/parity tests are listed below. Two parallel-agent leftovers remain: CRM Events still comments that Core has no redelivery age, and ADR-0019 still describes the pre-CORE-01 long refresh transaction.

## Task verdicts

| Task | Verdict | Why |
| --- | --- | --- |
| CORE-01 | **pass** | Claim/lease/finalize in `token_provider.go` commits before `gateway.Refresh`. Version fence, live-lease skip in `MarkReauthRequired`, and `SaveInstallation` lease reset match ADR-0017. Migration `000012`. Steal-after-TTL and crash-before-persist are documented residuals, not a remaining long TX. |
| CORE-02 | **pass** | CLI disable/enable-installation, revoke, uninstall. Uninstall sets `uninstalled` then `List`+`DeleteWebhook`; tests keep credentials/jobs/deliveries and close existing guards. Reconcile deletes stale destinations; inactive reconcile is permanent `installation_not_active`. No data purge, no new migration, ADR-0018. |
| CORE-03 | **pass** | `internal/oauthlimit` on start/callback only. 429 body identical for known vs unknown code/state. Activity `reauth_required` is 403 without `WWW-Authenticate`. OpenAPI updated. Inventory in `core-03-errors.md`. OAuth limiter env is loaded and validated in `config.go` (merge with CORE-06 succeeded). |
| CORE-04 | **pass** | No Redis, no PG token-bucket, no second limiter. ADR-0019 records “изменение не требуется”. `TestEnrichmentAndDirectoryShareExistingClientBudget` proves Notes/Tasks/Pipelines/Entities/directory share the 7/7 client. Token endpoint stays out of v4. |
| CORE-05 | **pass** | Worker, downtime reclaim, and `activity-control retry` all key off `created_at` + 7d. Maintenance GC and CRM Events `000008` tombstones reject late Apply of a collected `command_id`. `list`/`inspect` added; retry is the existing audited command. Schema gap `000013` is unused and the custom migrator allows it. |
| CORE-06 | **conditional** | Fail-fast validation, secrets-rotation runbook, and ADR-0021 (KMS deferred) are in tree. Target-server roles, listeners, `APP_ENV`, and secret store were not verified (BASE-01). That is required by the plan and correctly left open. |

CORE-07: coordinator ran `activity-ci` (215 Go PASS / 3 helper SKIP / UI 25) and `integration-test` (lead-status, webhook, widget routes, OAuth, uninstall). First integration-test FAIL on `TestTokenProviderPersistsPendingRotationWithoutSecondRefresh` was a test counter rolling back with the UPDATE; fixed with a sequence and re-run PASS.

## Checks run here (no docker-compose / make activity-ci / make test)

- `gofmt -l` on the stage-7 Go files listed in git status: empty (formatted).
- `go test -count=1 -timeout 60s ./internal/oauthlimit ./internal/platform/config ./cmd/activity-control ./cmd/integrations`: PASS.
- `go test ./internal/activitybridge -run 'TestPastRedeliveryHorizon|TestDeliveryExpiredError|TestStableErrorMapping|TestPublicInput'`: PASS.
- `go test ./internal/integrations -run 'TestCommandValidate|TestInstallationTransition'`: PASS.
- `go test ./internal/webhook -run 'TestPlanManaged|TestApplyWebhook|TestDeleteWebhook'`: PASS.
- `go test ./internal/oauth -run 'TestStart|TestCallback|TestLimiter'`: PASS.
- `go test ./internal/integration/amocrm -run 'TestEnrichmentAndDirectoryShareExistingClientBudget|TestSharedBudgetAcrossExistingWidgetsAndEvents'`: PASS (httptest, no PostgreSQL).

PostgreSQL suites (`token_provider_integration_test.go`, uninstall/reconcile integration, delivery horizon, cleanup, CRM Events tombstones) compiled in tree; they were not executed here.

## What landed vs the plan

**CORE-01.** `refreshIfVersion` is a poll loop: short `FOR UPDATE` claim of `lease_token`/`lease_until`, commit, HTTP, then a new short persist TX. Pending rotations stay in process memory and persist without a second Refresh. `loadCredential` now reads the lease pair, so `000012` must be applied before the new binary.

**CORE-02.** Operator actions are installation-scoped under an advisory lock. Uninstall is idempotent (`uninstalled` → `uninstalled`) and `cmd/integrations` always retries `webhook.Unregister` after commit. `StoredAccessTokenProvider` loads ciphertext even when status is not `active`; `RefreshIfCurrent` is a no-op (expired access token → documented retry).

**CORE-03.** Limiter keys are SHA-256 of IP and clipped `integration_code`/`state`. `X-Forwarded-For` is ignored. Activity `writeError` emits `code`/`message`/`request_id`/`retryable` and maps `reauth_required` to 403. Lead-status URLs, job types, and idempotency were not modified.

**CORE-04.** Code of `amocrm.Client` rates was not changed. The new test drives 28 mixed enrichment calls through one client and then `GetDirectory` on a burst-1 limiter.

**CORE-05.** `DeliverOne` expires aged pending/dead-lease rows before claiming; claim SQL also requires `created_at >= now()-7d`; a Go-side check expires a just-claimed row without sending. `RetryDelivery` returns typed `conflict` and does not reset attempts. CRM Events GC writes `event_command_tombstones` before deleting inbox/operations; a trigger rejects INSERT of a tombstoned `command_id`.

**CORE-06.** New fail-fast checks: HTTP listen address, OAuth state TTL, widget JWT leeway/lifetime, job timeout, OAuth limiter rates/TTL. `service-certs` is marked development-only. Production KMS is explicitly not chosen.

## Issues

### Issue 1 -- Severity: suggestion
- File: internal/services/crmevents/service.go:17
- Description: Parallel-agent leftover. `TechnicalHistoryHorizon` still comments “Paused jobs, operations and inbox receipts are kept: Core has no maximum redelivery age yet.” CORE-05 now GCs terminal inbox/operations after the same 7-day `created_at` floor and Core will not redeliver past that floor. The comment is false and will mislead the next owner change.
- Suggestion: State that paused/non-terminal rows are kept, terminal inbox/operations follow `HistoryHorizon`, and Core will not redeliver after that age.
- Status: fixed

### Issue 2 -- Severity: suggestion
- File: internal/services/crmevents/postgres.go:52
- Description: Same leftover on the Apply identity path: “Core can replay after an arbitrarily long executor outage or operator retry.” After CORE-05 that is no longer true. The SELECT is still required for in-horizon replay; the comment claims unbounded replay.
- Suggestion: Describe in-horizon inbox replay and tombstone rejection after GC, not unbounded Core replay.
- Status: fixed

### Issue 3 -- Severity: suggestion
- File: docs/adr/0019-gateway-outbound-budget.md:57
- Description: ADR-0019 still says concurrent Refresh is serialized by `FOR UPDATE` “сейчас транзакция удерживается на время сети — остаток CORE-01”, then adds “После CORE-01 короткий lease…”. After this tree, the long TX is gone. A reader of the outbound-budget ADR will think CORE-01 is still open.
- Suggestion: Replace the parenthetical with the lease from ADR-0017. Keep the point that the token endpoint is outside v4.
- Status: fixed

### Issue 4 -- Severity: suggestion
- File: internal/oauth/token_provider.go:291
- Description: `finalizeRotated` reuses `loadCredential`, which requires `installations.status IN ('active','authorizing')`. A remote-successful refresh therefore cannot persist if status left that set during HTTP (operator disable/uninstall, or `MarkReauthRequired` after a steal). ADR-0017 says in-flight refresh should finalize or itself mark 401. Lease TTL is hardcoded 15s; `AMOCRM_REQUEST_TIMEOUT` is any positive duration (default 10s). If timeout exceeds the lease, worker B can Refresh the same one-time token, get `invalid_grant`, release, mark `reauth_required`, and worker A’s persist then fails. Default compose is 10s < 15s, so this is config-dependent, not the default path.
- Suggestion: Persist against the claimed `token_version` without the live-status filter (still refuse if version moved), or set lease TTL ≥ outbound timeout and skip `MarkReauthRequired` while any lease of that version exists including a just-expired in-flight claimer.
- Status: fixed

### Issue 5 -- Severity: suggestion
- File: internal/services/crmevents/postgres.go:80
- Description: Tombstone trigger raises `23505` (`expired command replay is not accepted`). `Apply` returns that error raw. Tests (`TestTechnicalHistoryReplayWithinHorizonAndAfterGC`, `TestTechnicalHistoryReplayAfterGCDoesNotRollbackNewerSettings`) only check `err != nil` plus `retention_days` unchanged. Settings are not rolled back (the TX aborts), but a late Apply is not a typed `conflict`, and a connection error would satisfy the same assertion. `permanent()` in Core delivery does not treat a raw unique-violation as permanent.
- Suggestion: Map tombstone/unique identity to `serviceapi.Conflict` and assert that code in the GC replay tests.
- Status: fixed

### Issue 6 -- Severity: suggestion
- File: internal/platform/config/config_test.go:209
- Description: CORE-03 added `OAUTH_IP_RATE_PER_SECOND` and siblings; CORE-06 validates them (TTL must cover burst refill). `TestAPIRejectsInvalidOAuthAndJWTDeadlines` still only covers `OAUTH_STATE_TTL` and JWT. Zero/NaN OAuth limiter rates would fail `LoadAPI` and `oauthlimit.New`, but there is no LoadAPI case locking that.
- Suggestion: Add the same table-driven rejects used for widget/webhook limiter env (`OAUTH_IP_RATE_PER_SECOND=0`, TTL shorter than burst refill).
- Status: fixed

### Issue 7 -- Severity: suggestion
- File: internal/webhook/reconcile.go:24
- Description: Package comments on `Gateway` and `StoredAccessTokenProvider` (`internal/webhook/tokens.go:15`) narrate CORE-02/CORE-01 ownership rather than an invariant. Appendix G: comments explain why, not which task added the file.
- Suggestion: Keep the behavioral constraint (“does not refresh; disabled/uninstalled rows must still list+delete”) and drop the task IDs.
- Status: fixed

### Issue 8 -- Severity: nit
- File: internal/integration/amocrm/metrics_test.go:118
- Description: Shared-budget coverage for enrichment is Events/Notes/Tasks/Pipelines/Entities plus a follow-up `GetDirectory`. `ListCustomFields` (up to 20 pages per claim, also `DoJSON`) is not in the mix. Unlikely to have a second limiter; it is the heaviest enrichment path in ADR-0019.
- Suggestion: One CustomFields page in the same 7/7 burst test, or state in CORE-04 that CustomFields is covered by `DoJSON` only.
- Status: fixed

## CORE-07 — existing tests that must still pass

Coordinator has not run this gate. Do not add a fourth copy of these scenarios. After CORE-01..06, at least:

| Area | Existing tests |
| --- | --- |
| Lead-status admission → job → worker, replay, source-state fence | `internal/services/leadstatus/lead_status_workflow_integration_test.go` (`TestLeadSetStatusWorkflowCompareBeforeWrite`, `RetryObservesAppliedState`, `RechecksTenantBeforePatch`, `RejectsStaleLeaseAttempt`); `action_store_integration_test.go`; `capabilities_integration_test.go`; `module_integration_test.go` (`TestModuleResumesLegacyJobAndReplaysLegacyIdempotencyReceipt`); `handler_integration_test.go` (URLs 202/idempotency) |
| Webhook durable ingress and workflow rules | `internal/webhook/workflow_integration_test.go` (`TestLeadStatusWebhookDispatchIsDurablyDeduplicated`, `TestLeadStatusWorkflowDoesNotOverwriteChangedSourceState`, uncertain/applied correlation) |
| Widget bootstrap/ping, JWT/CORS, isolation | `cmd/api/widget_routes_integration_test.go` (`TestWidgetRoutesRateLimitBeforeConsumptionAcrossIntegrations`); widget auth middleware tests (`TestMiddlewareLogsSafeReasonAndRequestIDOnly` and neighbours) |
| Embedded/gRPC parity and durable work after upgrade | `internal/servicerpc/stage6_parity_test.go`; `internal/componentruntime/process_integration_test.go` (`TestComponentProcessesAndModeSwitch`); `process_faults_integration_test.go` (`TestComponentOSProcessFaults`) |
| OAuth lease (new CORE-01 cases, need Postgres) | `TestTokenProviderCoordinatesConcurrentRefreshAcrossProviders`, `TestTokenProviderStealsExpiredRefreshLease`, `TestTokenProviderPersistsPendingRotationWithoutSecondRefresh`, existing `TestUnauthorizedRefreshDoesNotDeadlockReauthorization` |
| Horizon / uninstall (need Postgres) | `TestWorkerDoesNotDeliverPastRedeliveryHorizon`, `TestOperatorRetryRejectsAgedFailedCommand`, `TestCleanupTechnicalHistoryHonorsHorizonAndStaysBounded`, `TestTechnicalHistoryReplayAfterGCDoesNotRollbackNewerSettings`, `TestUninstallPreservesOwnerDataAndIsDeniedByExistingGuards` |

`make activity-ci` remains coordinator-owned.

## Residual (not defects)

- Production SSH, live amoCRM, reverse-proxy ingress, and target-server role/listener/`APP_ENV` checks are open (BASE-01 / CORE-03 / CORE-06). Local Docker does not close them.
- CORE-07 not run.
- Crash after remote Refresh success and before persist still requires reauthorization (ADR-0017). In-memory pending does not survive process death.
- Uninstall uses the stored access token without refresh. Expired access → `webhook_status=error`; rerun uninstall. Remote amoCRM OAuth revoke API is not called.
- Uncertain/prepared/applied `outbound_effects` are kept. CORE-02 did webhook unregister, not effect reconcile.
- Tombstones last 7 days after receipt GC. After that a direct Apply of the old `command_id` can become a new accept; Core will not deliver it.
- Core `000013` is unused; the in-repo migrator keys on filename sort, not contiguous integers. Down migrations do not restore deleted rows.
- ADR-0020 was not written (number skipped; 0019 → 0021).
- Worker cleanup policy leaves `RedeliveryHorizon` zero; `effectiveHorizon` floors it at 7 days.
- Widget limiter / webhook / lead-status text/plain envelopes were inventory-only (CORE-03).
