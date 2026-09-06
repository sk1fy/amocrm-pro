# CRM Events v0 owner

`Service` implements `serviceapi.CRMEvents` identically for embedded and gRPC adapters.
`Service` depends on its own `Repository`, a Core Policy port and the Gateway
port. `NewPostgres` implements complete atomic owner operations; `New` is the
composition wrapper that supplies this adapter using only its own pool. It never imports Core repositories, OAuth/token providers, an amoCRM HTTP
client, Activity implementation, generated protobufs, or Core jobs.

The composition root starts `Run(ctx)` once per configured executor; this runs
an owner scheduler and two page workers by default. `Schedule`, `RunOnce`, and
`Retain` are separately callable for deterministic operations/tests. Multiple
schedulers are protected by source locks and a partial unique index. Source
leases and monotonically increasing fencing tokens protect page commits after
restart/release, lease expiry, and explicit consumer disable. A short PostgreSQL
advisory admission lock limits active backfill pages to one across replicas;
current collection has priority over backfill. The total worker and connection
quota must still be counted across process replicas by the deployment owner.

Migrations live exclusively in `migrations/crmevents`. The migration role runs
those migrations before startup; the service runtime is not given DDL privileges
or another owner's DSN. `event_inbox`, operations, continuations, consumers and
events all belong to this database. Command receipts and jobs are deliberately
not purged in v0: deleting them needs a coordinated dedup/retry retention policy.

## Scan and recovery contract

* `sync`/`enable` activates the Activity consumer durably, defaults to a two-day
  initial scan, and resumes existing unfinished fixed work. Initial depth is
  1–7 days, retention 1–90 days (Activity uses narrower product bounds).
* `backfill` requires an active consumer, a past range within its retention, and
  at most 31 days. `disable` pauses the consumer and invalidates an in-flight
  writer. Other authorized consumers can be introduced through an explicit
  future contract; v0 deliberately admits only `activity`.
* Every claim reads exactly one Events API page, with `limit=100`. A scan freezes
  its target and one-hour windows, preserves page/pass/digest in its own job,
  and uses a one-minute overlap when moving between windows. Query boundaries
  are sent to amoCRM as Unix seconds; API documentation does not promise a
  snapshot or explicitly define inclusive boundary semantics.
* A fully traversed window is replayed until two consecutive complete traversal
  fingerprints match. Default limits are three passes and 1,000 pages per pass.
  A changing traversal becomes `pagination_unstable`; an excessively dense
  window becomes `page_limit_exceeded`. Neither advances the checked range.
* `verification=stabilized_api_scan` describes authorization-visible API scans.
  Equal replay fingerprints are evidence of stability, not proof of an immutable
  snapshot or access to all account history. Reordering can conservatively fail
  verification; additions invisible to both scans cannot be detected. Windows
  after verification are overlapped by later polling, but arbitrarily late
  events outside overlap require explicit backfill.
* Event writes, counters, progress, next page/pass/window, and operation status
  commit atomically. The source row is fenced again at write time. Network calls
  happen outside the write transaction with a default ten-second deadline;
  leases default to thirty seconds. The next slice always obtains a new live
  policy grant. Policy outage fails closed; disabling policy prevents the next
  slice while an already authorized call may finish within its deadline.
* Temporary errors have at most five attempts per page with capped backoff.
  `reauth_required` and denied policy pause work. Exhausted retries and unstable
  pages require an explicit `sync` after remediation; the cursor is preserved.
* Empty verified windows advance checked progress. Disjoint backfill islands
  remain separate coverage and cannot close a gap. Last event, last successful
  window, continuous checked period and retained history are separate fields.
* Retention deletes at most 1,000 rows per pass, updates the retained boundary,
  and never rewinds synchronization progress. History counters may therefore
  exceed currently retained row counts.

`Collector()` exposes owner backlog/state, oldest job age, max source lag and
persisted processed/inserted/updated/deduplicated totals without ID labels. SQL
and pool metrics are supplied by the composition root. Full payloads are not
logged.

## Owner tests

Set `CRM_EVENTS_TEST_DATABASE_URL` to an expendable PostgreSQL database whose
name ends in `_test`, then run:

```sh
go test -race ./internal/services/crmevents -count=1
```

The helper applies only owner migrations and truncates only this owner database.
No URL means integration tests are explicitly skipped. Pure unit tests still run.
Tests cover atomic rollback, replay/idempotency, repeated and changed events,
empty windows, equal timestamps, restart, shifting/unstable pagination, lease
loss, competing schedulers/workers, priority/backfill admission, policy outage,
reauth, retention, disjoint coverage and installation isolation. Transport and
separate process checks live at the composition boundary; real amoCRM E2E is a
separate verification and is not implied by these fixtures.
