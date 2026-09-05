# ADR-0008: Multi-widget provisioning and capability boundary

Status: Accepted.

Date: 2026-09-05.

## Context

The data model already separates an amoCRM integration (widget identity and
OAuth credentials) from an installation (that integration in one account).
Several integrations may share one account. Tenant isolation alone does not
authorize every product capability available in the worker binary.

The environment bootstrap also used to update integration configuration and
reactivate a disabled integration whenever API started. This made operator
disable and secret rotation unreliable.

## Decision

- Keep API and worker as the existing deployment units. A service is a business
  capability, not automatically an independent process.
- Provide an operator CLI using the existing database and encryption keyring.
  The CLI creates integrations, changes configuration, rotates secrets and
  enables/disables integrations and their capabilities. Administrative changes
  and their audit records commit atomically. Secrets enter via stdin and never
  appear in normal command output or audit metadata.
- Treat environment bootstrap as create-only. It must not undo subsequent
  operator configuration, capability revocation or lifecycle changes.
- Store grants in `integration_services`, keyed by integration and a stable
  service code. Missing or disabled grants deny execution. The initial catalog
  contains `lead-status`; infrastructure ping has no product capability.
- Migration 000007 explicitly grants `lead-status` to integrations already
  present, preserving existing installations during upgrade. Newly provisioned
  integrations have no implicit product grants. Initial environment bootstrap
  explicitly grants the existing product only when it creates the integration.
- Keep account-specific rule configuration scoped by installation. Do not add
  installation-level capability overrides until their policy is needed. An
  integration-level disable applies to every installation of that integration.
- Resolve tenant identity from verified JWT claims, OAuth state, or trusted
  persisted installation/job records. Never accept it from command JSON.
- Check tenant activity and capability inside action admission, before consuming
  the JWT replay record or idempotency key. Return HTTP 403 with
  `{"error":{"code":"service_not_enabled"}}` for denied product admission.
- Repeat authorization for worker execution and immediately before mutation,
  including webhook-origin lead-status workflows. Hold the capability and tenant
  locks through mutation so revocation and mutation have a defined order. Once
  disable commits, a new mutation cannot pass the guard. An already authorized
  mutation can finish before the disable operation commits.
- Preserve current public route and durable job names. Module extraction and
  namespaced route/job versioning require an explicit compatibility migration.

## Consequences

Operator disable preserves installations, queued jobs and history; it does not
uninstall a widget or unregister remote webhooks. Jobs denied by the execution
guard terminate according to the existing permanent authorization failure
contract. Re-enabling a capability does not replay terminal jobs automatically.
Historical job reads remain actor- and installation-scoped; revocation prevents
new product work rather than deleting receipts.

Webhook URL keys identify the installation. An amoCRM webhook body from a shared
account does not independently identify which widget sent it; account validation
therefore cannot distinguish two integrations in that account. Capability checks
use the installation resolved by the key, not a claimed widget identity in JSON.

The CLI is trusted infrastructure, requiring database and keyring access. Its
actor identifier is an audit attribution, not a replacement for host/operator
authentication. Runtime deployments must not expose it as a public endpoint.

Capability mutation uses database locking consistently with worker authorization.
This deliberately retains the current bounded mutation transaction; moving
external OAuth refresh calls out of transactions is a separate concurrency
change and is not implied by this ADR.

Bounded cleanup also removes consumed and unused OAuth states after expiry plus
the existing safety margin. This does not set a retention policy for audit,
completed jobs or workflow history.

Queue fairness, per-service metrics, full uninstall/revocation, webhook key
rotation, general JSON errors/settings and installed-widget browser E2E remain
separate work. Physical service separation needs evidence of an independent
scaling, release, SLA or ownership requirement.
