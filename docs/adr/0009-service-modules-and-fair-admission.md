# ADR-0009: Service modules and fair admission

Status: Accepted.

Date: 2026-09-05.

## Context

Provisioning and capability authorization separate integrations securely, but
lead-status still mixed product logic with transport and queue infrastructure.
Global priority claiming also lets a large integration repeatedly occupy every
worker slot. The capacity regression reproduces this with 100,000 high-priority
jobs for A and eight jobs for B in the same account, continuously replenishing A:
the original selector gives B zero of the first 24 occupied slots.

## Decision

### Service ownership

`internal/services/leadstatus` owns commands, validation, HTTP handlers, public
result projections, rule configuration, workflow routing, outbound effects and
job failure handling. Composition roots explicitly register its HTTP routes,
worker handlers, result decoders and transactional event router. Product modules
depend on the platform; `widgetapi` and `webhook` do not import product modules.

`widgetapi` owns generic atomic admission, tenant/actor/lease authorization,
idempotency and actor-scoped job reads. `webhook` owns durable ingress, parsing,
inbox processing and transactional dispatch via `services.EventRouter`. Unknown
jobs remain hidden from widget polling unless a module registers a public result
decoder. Registrations finish before serving requests and reject duplicates.

Preserve the existing external URLs, job types, command/result JSON and
idempotency scopes/hashes. Extraction does not require migration of queued work.
A regression seeds the previous durable representation and verifies replay and
execution through the new module registrations.

### Widget API limits

Every widget endpoint shares process-local token buckets for integration and
installation. Defaults are 100 requests/second with burst 200 per integration,
and 10 requests/second with burst 20 per installation. Budgets are checked and
spent together; a request denied by either budget spends neither.

The chain is CORS, JWT verification, exact issuer binding, rate admission, then
read-token consumption or the action's existing atomic admission. HTTP 429
returns JSON `{"error":{"code":"rate_limited"}}` and `Retry-After`, exposed
through CORS. A 429 spends no JWT, idempotency key or job; its JWT can be retried
while still valid. Preflights do not allocate tenant budgets.

Each cache has at most 10,000 entries, evicts idle buckets after 10 minutes, and
rejects new entries at capacity. Sweep work is throttled; full-cache traffic
cannot force a complete scan on every request. Idle TTL must cover full burst
refill so eviction cannot reset an exhausted budget early. Metric labels contain
fixed scopes/outcomes, never tenant IDs or arbitrary request values.

These limits protect authenticated downstream work. They are per API process;
replica count and restarts affect total budgets. They do not rate-limit the
preceding CORS/JWT database lookup or replace edge/OAuth ingress protection.

### Durable queue fairness

Migration 000009 adds a scheduling row per integration and one platform row for
jobs without an installation. Claiming rotates the least recently served lane,
preserving priority/run-after order inside that integration. Rotation persists
across batches and worker restarts.

`WORKER_INTEGRATION_CONCURRENCY`, default 2, caps live processing leases across
all installations and replicas for an integration. The platform lane has the
same independent cap. A short PostgreSQL transaction advisory lock serializes
claimers; capacity is read in a fresh READ COMMITTED statement after acquiring
the lock. No external calls happen under this scheduling lock. Existing bounded
reaping, failure observers, attempts and lease fencing remain transactional.

All workers must use the fair claimant with the same cap. The older Store claim
methods remain for compatibility and regression comparison; the runtime config
requires a positive cap. Mixed old/new workers do not provide the cluster cap.
Deployments must drain/stop old workers before starting the new worker pool.

The cap bounds live leases, not remote requests continuing after a lease expires.
Existing mutation authorization and fencing remain required. A lone integration
may leave worker slots unused; raise the cap only with measured capacity needs.

The scheduler is serialized and candidate lookup scales with installation count
and batch size. The capacity test demonstrates isolation under a skewed backlog,
not unlimited throughput across arbitrarily many installations or a wall-clock
SLO. The same workload gives B all eight completions within 16 occupied slots
under fair claiming. A separate test uses eight simultaneous claimants to verify
the common cap, including multiple installations and platform jobs.

### Observability

Keep existing aggregate backlog metrics and add backlog/oldest-ready age by
bounded service (`lead-status`, `platform`, `other`), plus execution duration by
service and outcome. Catalog job types map to their service; unknown persisted
types aggregate to `other`. No raw job type, integration ID or installation ID
becomes a new metric label. Per-integration investigation uses read-only SQL.

## Consequences

Service ownership and noisy-neighbor protection no longer require separate
deployments or Redis. Physical service splitting, distributed ingress quotas,
settings/general error contracts and the remaining production lifecycle remain
separate decisions. Configuration, rollout and reproduction instructions are in
the [capacity runbook](../runbooks/widget-capacity.md).
