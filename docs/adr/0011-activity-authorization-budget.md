# ADR 0011: Activity authorization budget and delegation freshness

Status: accepted, 2026-09-07.

Opening one Activity panel previously fetched the actor's live amoCRM role four
times: Core Policy `Issue`, Activity `Validate`, Gateway `Validate`, and CRM Events
`Validate`. These calls share the same Gateway-owned amoCRM budget with background
collection and existing jobs. A cold directory additionally needs account/users
reads. Repeating the role check at every hop consumed budget without changing the
actor or the request.

## Decision

Every new Core ingress request and every asynchronous command delivery obtains a
new delegation through Policy `Issue`. Issue performs a live Core DB admission
check and, for user actions, one live `GetUserAuthorization` through the existing
amoCRM client and shared limiter. Request IDs are not cache keys. There is no
cross-request role cache. An inactive or non-admin actor is denied at the next
Issue, even when the directory cache is warm.

The signed delegation certifies that observation for at most 30 seconds. Validate
checks signature, issuer, audience, action, expiration and authenticated service
identity, then queries current Core DB policy on EVERY hop. Pilot, capability,
integration status, installation status and `reauth_required` are never cached.
Their revocation prevents the next Validate, including Gateway access. DB failure
fails closed as unavailable; it is not an authorization denial.

A role revoked in amoCRM after successful Issue can still finish an already
issued chain until that delegation expires (maximum 30 seconds). This replaces
hop-by-hop immediate role revocation with an explicit bounded snapshot, not with
a claim of immediate revocation. Fresh requests and retried outbox deliveries
recheck the role. No token lifetime extension or clock leeway is introduced:
Issue and Validate use the same authoritative Core Policy clock in both embedded
and gRPC mode. Product container clock skew is not grounds for weakening JWT
validation.

`DelegationChecker` is an explicit optional interface for local DB revocation.
The production database checker implements it. Custom `LiveChecker` adapters
without it retain the conservative full check on Validate. Tests of those legacy
adapters alone do not prove the production request budget or snapshot behavior.

Core grants are scoped to the exact target operation. A panel receives only
Activity/panel, Events/read and Gateway/users; it cannot change settings or start
collection. Settings, status, sync delivery and operation lookups receive their
own grants. The existing settings contract uses the same action for reading and
configuration; splitting that action requires a separately versioned contract.
The old broad helper remains wire/client compatibility support, not the default
for new Core ingress. Activity's mTLS identity can call only CRM Events QueryEvents,
Status and OperationStatus, never Apply or an unknown future method. A token's
`jti` is not consumed once: one delegation intentionally crosses multiple services.

## Verification and limits

`TestPanelUsesOneLiveActorLookupAndImmediateDatabaseRevocation` exercises the
production database checker, real Activity/Gateway/CRM Events service methods and
actual amoCRM client over a fixture HTTP transport. Two successful panels make
exactly two `/api/v4/users/7` calls (one per Issue); the first also makes two directory
reads and the second uses the directory cache. It checks fresh Issue denial after
admin revocation, acceptance of a still-valid role snapshot, and immediate
Issue/Validate denial for every Core DB revocation class. Product repositories
and upstream HTTP responses are fixtures; this is not a real amoCRM load test.
Existing signature/expiry tests retain the 30-second expiration enforcement.
`TestCRMEventsMTLSActivityCannotApply` checks the real mTLS transport ACL and
confirms a denied Apply never reaches the receiver.

Lower role request volume reduces budget pressure. It does not establish a p95
latency SLO or prove starvation impossible under production load. Directory cache
misses, amoCRM latency, retries, live role checks on separate browser requests and
background collection still share the existing limiter.
