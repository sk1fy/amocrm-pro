# CORE-05: finite technical history and calendar redelivery horizon

Date: 2026-09-10. Horizon: **7 days from command `created_at`** (not attempt
count, not last-retry window). Same value as ADR-0016 `HistoryHorizon`.

**Audit fix:** terminal job GC now checks every RESTRICT reference, including
idempotency keys and rule configuration results. Old rule configurations are
collected in a bounded pass before jobs, only after both their own and their
job's retention windows expire, and only when no receipt/effect still needs
them. New metric record kind: `rule_configuration`. Migration 000015 adds the
cleanup index. This prevents one completed rule job from rolling back the
entire maintenance transaction. [Verification](fixes/README.md).

## Enforcement

| Path | Behavior |
| --- | --- |
| Worker `DeliverOne` | Expire pending/dead-lease rows with `created_at` older than 7d; never claim/send them. Atomic claim also requires attempts < max_attempts, even when exhausted rows exceed the 100-row cleanup batch |
| Recovery after downtime | Same SQL age filter after restart; no unbounded catch-up |
| `activity-control retry` | Typed `conflict` (`ErrDeliveryExpired`); attempts not reset |
| CRM Events `Retain` | After that floor, GC terminal inbox/operations; tombstone rejects late Apply |

## Core GC (`maintenance.Cleanup`, lock `6584483612447211903`)

Collected, bounded by existing batch/max-batches: terminal jobs (+ cascaded
attempts), audit_log, webhook tombstones (90d last_seen, never shorter than
payload retention), terminal workflow_runs without remaining effects, terminal
outbound_effects with known outcome, terminal Core outbox receipts.

Not collected: `outbound_effects` in `prepared`/`applied`/`uncertain` (unknown
external mutation; CORE-02); webhook payloads earlier than ADR-0006; Activity
owner `command_receipts` (other database; Core will not redeliver past horizon);
paused CRM Events jobs.

## CLI

`activity-control list|inspect` for failed/expired deliveries. Retry remains the
existing audited command with the age guard. Integrations CLI unchanged.

## Tests (compile here; PostgreSQL suites need DSN)

- `TestDeliverySkipsExhaustedBeyondCleanupBatch` (101 exhausted rows plus one eligible command, both pending and expired delivering states)
- `TestPastRedeliveryHorizonUsesCreatedAtNotAttempts`
- `TestWorkerDoesNotDeliverPastRedeliveryHorizon`
- `TestOperatorRetryRejectsAgedFailedCommand`
- `TestDeliveryRevocationAndAuditedRetry` (still in-horizon)
- `TestCleanupTechnicalHistoryHonorsHorizonAndStaysBounded`
- `TestTechnicalHistoryReplayAfterGCDoesNotRollbackNewerSettings`
- `TestTechnicalHistoryReplayWithinHorizonAndAfterGC`

## Residual

- Uncertain/prepared/applied effects stay until CORE-02 reconcile.
- Activity module receipts are not GCd from Core.
- Tombstones older than 90d may allow a very late identical webhook; lead-status
  is convergent. Non-commutative future workflows must not rely on that.
- Down migrations do not restore deleted rows.
- Schema: Core `000014_core_redelivery_horizon`, CRM Events `000008_command_identity_horizon`.
