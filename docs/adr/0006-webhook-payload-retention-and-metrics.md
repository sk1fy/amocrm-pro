# ADR 0006: bounded webhook payload retention with durable history

## Context

`webhook_deliveries.raw_body` and `inbox_events.payload` may contain PII and
grow without bound. PR #35 separated replay identity and workflow history into
`webhook_event_tombstones`, `workflow_runs`, and `outbound_effects`, so raw
payload rows no longer need to be permanent. Cleanup must remain PostgreSQL-only,
bounded, replica-safe, and observable.

## Decision

- The worker deletes terminal inbox events after `WEBHOOK_INBOX_RETENTION` and
  terminal deliveries after `WEBHOOK_DELIVERY_RETENTION`; both default to
  `720h` (30 days).
- Eligibility uses PostgreSQL time and a strict `updated_at < now() - retention`
  boundary. Inbox terminal states are `processed`, `failed`, `dead`, and
  `ignored`; delivery terminal states are `parsed`, `invalid`, and `failed`.
- Inbox events are deleted first. A delivery is deleted only when it has no
  remaining inbox child, preserving pending/processing work and satisfying the
  existing restrictive foreign key.
- The existing advisory lock, timeout, `SKIP LOCKED`, batch size, and maximum
  batch count bound every pass across worker replicas.
- Tombstones, workflow runs, outbound effects, jobs, and audit rows are not in
  this cleanup scope. Deleting an origin event therefore nulls the optional run
  link while retaining the copied origin hash and effect history.
- Cleanup pass outcomes, deleted rows, batch-limit pressure, workflow routing,
  and finalized workflow job attempts are exported through low-cardinality
  Prometheus metrics. Tenant, resource, payload, and error text are never labels.

## Consequences

The default raw-payload window is finite and independently configurable for the
two layers. A retained tombstone still suppresses a historical replay after its
payload rows are gone. Workflow history intentionally outlives payload data.
Counters reset with a worker process and dashboards must use `rate`/`increase`.
Finite tombstone/run/effect retention, exact backlog cardinality, load tests,
and SLO/alerts remain separate production work.
