# ADR 0007: async rule management principal and revision CAS

## Context

The webhook-origin lead-status workflow has a typed PostgreSQL rule, but no
safe management contract. A disposable widget JWT proves tenant and user
identity; it does not prove current amoCRM administrator rights. Configuration
is asynchronous, so the database mutation and generic job completion are
separate commits and a retry must not apply the same operation twice.

## Decision

- The management principal is the verified widget user. Admission atomically
  consumes the JWT, records the Idempotency-Key outcome, and enqueues a job with
  durable `widget_user/<amo user id>` ownership.
- The worker performs a live amoCRM user lookup after claim and requires
  `is_active && is_admin`. JWT claims never grant administrator authority.
- The source pipeline/status tuple is immutable rule identity. There is no hard
  delete; disabling a rule preserves history and workflow-run foreign keys.
- `expected_revision=0` is create-only and creates revision 1. A positive value
  updates only that exact revision and produces `N+1`. Every successful command
  consumes one revision, even if desired fields are otherwise unchanged.
- Rule mutation, a redacted audit record, and an immutable configuration receipt
  keyed by job ID commit in one PostgreSQL transaction after rechecking current
  lease, actor/resource ownership, and active tenant lifecycle.
- A retry first reads its receipt and returns the recorded typed snapshot. This
  closes the crash window between rule commit and generic job completion.
- This slice exposes only asynchronous configure and actor-scoped job status.
  Listing, hard deletion, generalized registries, and amoCRM catalog validation
  remain outside the contract.

## Consequences

Concurrent stale commands cannot overwrite each other; one exact CAS wins and
the others finish with `revision_conflict`. A revoked/non-admin user or inactive
tenant makes no rule mutation. The receipt is durable workflow history and is
not part of raw webhook payload cleanup. PostgreSQL remains the only durable
coordination mechanism and Redis is not introduced.

Issue: https://github.com/sk1fy/amocrm-pro/issues/37
