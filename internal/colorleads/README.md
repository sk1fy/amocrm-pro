# Color Leads product boundary (PV-04.02 / PV-04.04 candidate)

This package is deliberately **not wired into cmd/api** while PV-03 is being
implemented in another session. `schema.sql` is a candidate schema, not an
applied/numbered production migration. There is no production rollout here.

## Widget contract

Register `NewHandler(NewStore(pool, guard)).Register(router)` behind the real
`widgetauth.VerificationMiddleware`, origin restrictions and admission limits.
Do not use the consuming authentication middleware: this store consumes JWT jti
atomically with every successful read/write. A fresh SDK request token is needed
for each request. The account/installation identity comes only from the verified
principal; fields masquerading as identity at the request-envelope level fail.

Requests require `X-Routing-Epoch` and `X-Installation-Generation` positive integer
headers. These are untrusted expectations, not authority. CORS must explicitly
allow these names when actual route wiring is added.

* GET `/v1/widgets/color-leads/settings`: `{settings:{...},revision:N}`.
* PUT same path: `{settings:{...},expected_revision:N}`, returns the new document.
* POST `/v1/widgets/color-leads/colors/query`: `{lead_ids:[1,2]}`, returns
  `{colors:[...],revision:N}`. Empty/all-account enumeration is rejected.
* PUT `/v1/widgets/color-leads/colors`: `{colors:[...],expected_revision:N}`.
  Replaces the complete bounded snapshot, not a partial lead update.

Colors preserve the composite `(lead_id,stage_id,pipeline_id)` key and nullable
`borderColor`, `backgroundColor`, `timestamp`. Duplicate composite keys fail;
we deliberately do not guess how SSD's differing create/update keys should be
reconciled. Hex colors are six/eight digits. Max 1000 records per snapshot;
larger legacy accounts need an explicit paged import/storage design before use.
Settings remain lossless JSON (max 256 KiB, 500 rule objects), including unknown
legacy fields. Rule semantics and supported operators need inventory validation;
this store's structural validation is not rule-equivalence acceptance.

Errors are `{error:code}`: 400 invalid_payload, 401 unauthorized,
409 route_rejected/revision_conflict, 503 not_configured/temporarily_unavailable.
Unknown write outcomes must be resolved by authenticated revision read before
retrying; do not blindly repeat a replacement write.

## Mandatory PV-03 handoff

`Guard.Admit` must lock and validate authoritative product binding,
installation incarnation, active Core owner, routing epoch/generation,
entitlement and user permissions **inside the supplied transaction**. It must
retain these locks until commit, coordinating with cutover commands and old
owner dispatch admission. Lock order begins with installation/integration.
A nil guard denies access; a cached route lookup cannot satisfy the interface.
The generation is part of the document primary key so reinstall does not expose
previous incarnation data. Cross-account/integration identity is checked against
active installation rows before guard admission.

No metadata/leads Gateway endpoint is invented here. CRM reads, production
service registration, CORS, integration binding and frontend readGateway remain
PV-03/product wiring work. No browser/localStorage SSD fallback is authorized.
Stopping new legacy callbacks does not prove old observers/requests stopped;
Core activation requires authoritative drain evidence outside this package.

## Privileged migration command

`Store.ImportSnapshot(ctx, ImportGuard, ImportCommand)` accepts trusted command
identity (`InstallationID`, `Generation`, `Epoch`, `TransferID`, `Actor`, `SourceDigest`) and
`Snapshot{Settings, Colors}`. It is not a browser HTTP route. Caller must derive
actor/scope from authenticated operator context, never a submitted actor string.
`ImportGuard.AdmitImport` must validate and lock the frozen transfer, matching
incarnation/epoch and complete old owner/client drain evidence. It must reject
active routes. An existing product document cannot be overwritten by import.

Settings, colors and the import receipt commit atomically. Repeating the same
transfer/content/epoch returns `Replayed=true`; changed content conflicts. Digest
is SHA256 of Go `json.Marshal(Snapshot)` bytes (not the offline export archive
hash). Preserve raw source rows/metadata separately in the offline archive;
product rows intentionally do not retain SSD database primary keys. Required
`SourceDigest` binds the verified offline export SHA256 to the durable receipt; changed
archive digest on retry conflicts. The trusted import caller must verify archive
content using the exporter’s specified canonical serialization and its tenant
before invoking the command; a hash string alone proves
neither authenticity nor that the archive exists. This command
does not activate Core, issue receipts proving legacy drain, or perform rollback.
A production rollback policy must preserve and reconcile post-cutover writes.

For `scripts/color-leads/snapshot.py`, SourceDigest is `source_sha256`: SHA256
of the parsed export serialized with Python `json.dumps(ensure_ascii=False,
sort_keys=True, separators=(",", ":"), allow_nan=False)` encoded as UTF-8.
It is not the original pretty-printed file hash or the operational payload hash.

## Validation scope

`go test ./internal/colorleads` exercises HTTP identity/payload boundaries and
validators. `TestPostgresPersistenceCASAndImport` runs only with the repository's
explicit test-database configuration; it checks PostgreSQL CAS, JWT replay,
tenant isolation and atomic import receipts using a **fixture guard**. It does
not prove production PV-03 locking, legacy drain, live Gateway/browser behavior,
visual acceptance or imported-rule parity. Those are still required before use.
