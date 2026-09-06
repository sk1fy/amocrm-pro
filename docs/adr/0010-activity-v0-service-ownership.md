# ADR-0010: Activity v0 and separable CRM Events

Status: Accepted for implementation; runtime evidence is recorded separately.

Date: 2026-09-06. Baseline: `a33a845f2db23b46c96fc7199d1161f654ad86f5`.

The implementation scope is the owner's Activity architecture summary **v2**
(2026-09-06), superseding previous Activity proposals and appendices. PHP history
migration, work sessions, pauses, scenarios, Telegram, billing and production
cutover are excluded.

## Baseline audit and changes

Core already implements installations, encrypted OAuth credentials, token
refresh, disposable widget JWT verification, tenant-bound CORS, capability
admission, durable jobs, retries, leases, fencing and fair queue admission.
`leadstatus` registers HTTP/job/event adapters in Core composition roots.
Its public URLs, payloads, job types and storage remain compatible.

The existing queue cannot store CRM Events jobs without violating ownership:
`jobs.installation_id` references Core installations and the fair claimant joins
Core capability/lifecycle tables. CRM Events therefore reuses technical patterns,
but has an independent queue, scheduler, leases and transactions in its own DB.
It never consumes Core worker slots. Activity owns actual product settings and
command receipts, and reads raw data exclusively through the CRM Events port.

At baseline, only worker's `amocrm.Client` executes normal API v4 operations:
lead-status user/lead reads and prepared PATCH; webhook list/register/delete.
The API's OAuth gateway exchanges/refreshes tokens and reads account information
during OAuth. The in-memory limiter belongs to a client instance, not the
repository, a shared PostgreSQL instance or all Go processes.

## Owners and allowed dependencies

| Owner | Logical database | Outgoing application dependencies |
| --- | --- | --- |
| Core API | Core | Activity, CRM Events, Core policy |
| Core worker + Gateway/policy | Core | amoCRM; existing Core jobs |
| Activity | Activity | CRM Events, Core policy, bounded Gateway users |
| CRM Events | CRM Events | Gateway events, Core policy |

No service receives another owner's runtime pool. Runtime roles cannot CONNECT
to another owner's database and cannot create schema objects. Migration roles
are separate and are supplied only to migration containers. Separate logical
databases may share PostgreSQL CPU, storage and availability; this is not full
physical isolation. Credentials remain in Core. External IDs are references,
with no foreign keys, joins, FDW or transactions across owners.

Domain contracts live in `internal/serviceapi`; protobuf and transport adapters
are separate. Local and gRPC adapters call the same service implementation and
must enforce the same validation, authorization and idempotency. In gRPC mode
services require mTLS and Core-signed short-lived scoped delegation. Actor IDs,
installation and integration originate only in verified widget context.

## Placement and outgoing budget

The pilot runs **one Core worker/Gateway owner** and one API. Existing worker
callers retain the same client object as Gateway; Events and Activity never
construct amoCRM clients. The embedded service graph runs in Core API with
independent pools and Events executor; its Gateway/policy calls still reach the
single worker over mTLS because API and worker do not share memory.

In `grpc` mode the same Activity and CRM Events code runs in independent
containers with only its own DSN and mTLS identity. Core API does not receive
their DSNs. No automatic remote-to-local fallback is permitted. Mode switches
reuse the same owner databases and preserve jobs, operation IDs and inboxes.

The existing integration/account limiter remains the outbound budget owner.
OAuth bootstrap `GET /api/v4/account` also runs through the same worker client,
using a separate Core-only mTLS RPC. The API supplies the integration from the
consumed OAuth state and its transient access token; no product receives that
token and no new credential store/refresh mechanism is introduced. Known domains
resolve to account IDs across all Core integrations and share their exact bucket.
First account discovery uses a common bounded discovery bucket plus integration
budget, then debits the learned account before returning it for persistence.
Only `/oauth2/access_token` exchange/refresh, PHP and third-party traffic remain
outside this Go API v4 budget; 429 remains possible. Multiple Gateway replicas or
additional direct Go API v4 callers require coordinated quotas and measurements.

## Admission, data and operations

Activity requires an explicit Core installation pilot flag and integration
capability, both default off. Policy also checks active installation/integration
and server-verified actor rights. Reading other employees requires admin rights;
frontend `is_admin` never grants access. Each new work slice rechecks admission;
policy unavailability fails closed, and no browser JWT is retained in jobs.

Core atomically consumes the disposable token, creates a scoped idempotency
receipt and durable command outbox. HTTP 202 means platform admission. Delivery
reissues delegation after policy checks; recipient atomically deduplicates and
creates its own operation/job or setting. Unknown delivery results retry the
same command. Pending delivery, receiver admission, running, completed and final
failure are distinct; command identity is never changed for retries.

CRM Events owns fixed time windows, pagination cursors, continuous checked
coverage, events, history and retention. Empty windows advance coverage; partial
pages and disconnected backfills do not close gaps. One bounded page is fetched
outside the write transaction; fenced event writes, progress and continuation
commit together. Replays distinguish processed/inserted/updated/deduplicated.
Retention advances stored history bounds without resetting checked coverage.

The [official Events API](https://www.amocrm.ru/developers/content/crm_platform/events-and-notes)
was checked on 2026-09-06: maximum page size 100, page pagination and timestamp
from/to filters, with results limited by authorization. The API does not promise
snapshot pagination. Bounded replay/stabilization detects changes; even a stable
pass does not prove access to unavailable history or future late-visible events.

## Stages and evidence

A: baseline/owners; B: DB roles, ports and secure independent process path;
C: collection and recovery; D: panel/settings/admission/outbox;
E: contract/failure/load/switch tests; F: selected real installation.

Tests and runbooks must distinguish implemented behavior from executed evidence.
Real installed-widget E2E, deployment, production load, backup/restore and PHP
migration must never be inferred from mocked or embedded tests.
