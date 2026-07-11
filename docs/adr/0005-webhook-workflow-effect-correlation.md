# ADR 0005: webhook-origin workflows and durable desired-state correlation

- Status: accepted
- Date: 2026-07-11
- Issues: #10, #21

## Context

The webhook inbox durably normalizes amoCRM deliveries, but its handler was
observational only. The first widget workflow can change a lead status and the
resulting `status_lead` webhook has no caller-supplied operation identifier.
Relying on the inbox row itself for deduplication also prevents later removal of
raw deliveries without making an old delivery actionable again.

## Decision

The first webhook-origin workflow is an opt-in, typed lead status transition
rule. A rule maps one exact source `(pipeline_id, status_id)` to a distinct
target tuple. Matching `status_lead` events create one durable workflow run and
one convergent job. The job reads current lead state and patches only when the
target is not already present.

PostgreSQL remains the only coordination system:

- a compact webhook tombstone claims each normalized deduplication hash before
  creating an inbox event and is retained independently of raw payload rows;
- a workflow run is unique by installation, workflow version, and origin hash;
- an outbound effect intent is committed before remote mutation and records
  tenant, job, lead, canonical desired state, and a bounded observation window;
- a matching incoming status webhook may move a prepared, uncertain, or
  applied effect to observed and marks the event as an ignored self-effect;
- state updates never regress an already observed effect;
- ambiguous PATCH retry still performs GET/compare and does not send a second
  mutation when the target state is already present;
- source and target tuples must differ, so an unmatched late target webhook
  cannot recursively satisfy the same rule.

Tombstones and workflow identity outlive removable delivery/inbox payloads.
This slice does not delete them and therefore does not re-enable a historical
side effect.

## Correlation semantics

amoCRM does not echo an application operation ID in the webhook. Correlation is
therefore semantic, not cryptographic: exact tenant, lead, desired pipeline and
status, plus a bounded local receive-time window. The selected workflow is
convergent, so suppressing an identical concurrent human transition is harmless
to the target-state invariant. This technique must not be generalized to
non-commutative effects without a stronger upstream correlation mechanism.

## Consequences

The workflow gains durable loop prevention, auditability, and replay safety
without Redis. It also adds retained compact metadata. Product-facing rule CRUD,
raw delivery cleanup, and a future tombstone expiry policy remain separate work.
