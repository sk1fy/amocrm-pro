# Widget admission limits and queue capacity

## Deployment

Apply migration 000009 before the new worker starts. Drain/stop **all old worker
replicas**, then start the new worker pool with one consistent
`WORKER_INTEGRATION_CONCURRENCY`. Old workers call the legacy global selector and
would bypass the shared cap. No queued payload or HTTP URL migration is needed.

For the local Compose stack:

```sh
make build
docker-compose stop worker
make migrate
docker-compose up -d api worker
```

Use the corresponding stop/drain and deployment operation for every production
replica. The API may continue durably admitting work during worker replacement.
Before any schema rollback, stop every new worker; its fair claimant requires
`job_queue_lanes`. Follow the existing guarded migration rollback runbook rather
than dropping tables manually.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `WIDGET_INTEGRATION_RATE_PER_SECOND` | 100 | Shared widget requests per integration per API process |
| `WIDGET_INTEGRATION_BURST` | 200 | Maximum initial/refilled integration burst |
| `WIDGET_INSTALLATION_RATE_PER_SECOND` | 10 | Shared widget requests per installation per API process |
| `WIDGET_INSTALLATION_BURST` | 20 | Installation burst |
| `WIDGET_LIMITER_INACTIVE_TTL` | 10m | Idle bucket lifetime; must cover full burst refill |
| `WIDGET_LIMITER_MAX_ENTRIES` | 10000 | Maximum entries in each of the two caches |
| `WORKER_INTEGRATION_CONCURRENCY` | 2 | Cluster-wide live processing leases per integration; same cap for the separate platform lane |
| `WORKER_CONCURRENCY` | 4 | Local execution slots per worker process |

API limits apply to bootstrap, ping, product actions/rule configuration and job
polling together. Multiple accounts of one integration share its integration
budget. A denied request spends neither token bucket and does not consume its
disposable JWT or idempotency key. Respect `Retry-After` on HTTP 429. If the JWT
has expired by the retry, obtain a fresh one and keep the same idempotency key.

Budgets are process-local: adding API replicas multiplies possible aggregate
traffic, and restarting an API resets its buckets. Unauthenticated requests
still require upstream protection; this limiter only allocates verified tenant
budgets. At cache capacity, existing tenants keep their buckets while new entries
receive 429 until space is available.

Worker fairness rotates integrations regardless of their relative job priority;
priority still orders work within an integration. All its installations share
one lease budget. With only one busy integration and cap 2, increasing local
worker concurrency above 2 does not increase that integration's throughput.
Raising the cap trades reserved capacity for higher single-integration throughput.

## Metrics and diagnosis

- `amocrm_widget_limit_decisions_total{scope,outcome}` shows accepted and denied
  budget decisions. Rejections in both tenant budgets increment both scope
  counters; their sum is not a unique HTTP request count.
- `amocrm_widget_limit_entries{scope}` tracks integration/installation cache use.
- `amocrm_jobs_service_backlog{service,kind}` counts ready, scheduled, live
  processing and expired-lease work.
- `amocrm_jobs_service_oldest_ready_seconds{service}` measures waiting age from
  `run_after`; empty queues report zero.
- `amocrm_jobs_execution_duration_seconds{service,outcome}` measures finalized
  attempt execution time. It does not include queue waiting time.

Service labels are bounded to the catalog, `platform`, and `other`; arbitrary job
names and tenant IDs are not exported. Increasing ready age with persistent live
processing suggests saturation. Expired leases or scheduler claim timeout logs
require investigation before raising concurrency.

Read-only per-integration inspection in `make db-shell`:

```sql
SELECT i.code,
       count(*) FILTER (WHERE j.status IN ('queued','retry')
                         AND j.attempts < j.max_attempts
                         AND j.run_after <= statement_timestamp()) AS ready,
       count(*) FILTER (WHERE j.status='processing'
                         AND j.locked_until >= statement_timestamp()) AS live,
       min(j.run_after) FILTER (WHERE j.status IN ('queued','retry')
                                AND j.attempts < j.max_attempts
                                AND j.run_after <= statement_timestamp()) AS oldest_ready
FROM integrations i
JOIN installations a ON a.integration_id=i.id
JOIN jobs j ON j.installation_id=a.id
GROUP BY i.code
ORDER BY oldest_ready NULLS LAST;
```

## Reproducible capacity evidence

`make integration-test` includes `TestFairClaimCapacityNoisyNeighbor` in
`internal/jobs`: two integrations share an account, A starts with 100,000
higher-priority jobs and is continuously replenished, while B has eight jobs.
The old real selector gives B zero of 24 occupied slots; the fair selector gives
B eight completions within 16 slots. Synthetic completions isolate scheduling
behavior; timings are diagnostic, not a production throughput promise.

The same suite checks eight concurrent claimant replicas, shared caps across
multiple installations, platform jobs, rotation across single-slot polls,
expired leases, observer rollback and scheduler timeout. The API integration
test covers real JWT/CORS composition and verifies that 429 leaves the JWT,
idempotency key and job admission untouched.
