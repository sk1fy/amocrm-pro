# Company CRM read boundary

Status: accepted for P12.1. Read-only implementation. Production enablement is pending.

rakurscrm supplies the staff and client directories. Core owns its credential and
provides purpose-specific reads from the existing singleton worker. This connection
is independent of Activity grants and customer installation observations. It does
not change Platform lifecycle clients or import historical installation events.

The server configuration pins numeric company account ID, installation UUID,
integration UUID and explicit binding revision. The exact active non-fixture
installation must have stored OAuth credentials and an active integration. Token
refresh does not change binding revision. Every credential lookup and refresh
checks the pinned scope. The first provider read verifies `/api/v4/account` before
any directory read. Owner DB authority is rechecked before publishing a snapshot.
The same `amocrm.Client`, token provider and account/integration limiter are reused.
The existing single outbound worker replica restriction remains in force.

A separate private HTTPS listener uses `servicerpc.TLSFromFiles`, TLS 1.3, verified
client certificates and exact SPIFFE service URI authorization. Only
`spiffe://amocrm-pro/admin-company-directory` may read Staff. Only
`spiffe://amocrm-pro/platform-company-crm` may read Clients, WorkflowMetadata and
ValidateWorkflow. A certificate must contain exactly that one URI. CN, headers,
Admin tokens and TeamOS identities grant no access. The plaintext health and old
Admin CRM listeners do not mount these routes.

Each resource publishes only after every bounded provider page succeeds. Errors,
partial scans and binding changes retain the previous complete snapshot as stale,
including its original account and binding revision. No prior snapshot means null
data and incomplete error source. Consumers must compare source revision with the
revision they expect. Data from an old binding never becomes evidence of the new
binding. Observation time starts before scanning. Freshness is five minutes and
successful reads poll no faster than once a minute. Requests have ten-second
contexts, eight HTTP admission slots and one context-aware scan slot. Provider
429 Retry-After passes to source metadata and the HTTP header with no immediate
retry. Each list is bounded at forty pages of 250 rows. Exceeding bounds is an
incomplete read, never an empty successful catalog.

Staff activity is active, inactive or unknown. Missing provider activity remains
unknown. Missing staff rows do not prove deletion. Credentials and Admin grants
remain Admin-owned. Clients preserve company/contact IDs and contact-company
relations. Account mappings use only an explicitly configured field whose metadata
confirms numeric type. Values must be positive exact integers. Zero means no field
values; one means each listed account resolves to this company; multiple means an
account also occurs on another company; unknown means no verified mapping or
invalid value. Multiple customer accounts may belong to one company. Names,
emails, domains and Web fields never supply identity. Unresolved rows remain
companies and do not become runtime accounts.

Workflow metadata returns pipelines with stages and observed lead field metadata.
Required-field readiness remains unknown. ValidateWorkflow checks only binding
revision and stage membership in its pipeline, explicitly returns
`scope=pipeline_stage` and `write_ready=false`. It cannot authorize lead creation.
No lead POST, receipt, intent or schema migration is included.

Provider contracts were checked against official documentation on 2026-11-18:
[users](https://www.amocrm.ru/developers/content/crm_platform/users-api),
[companies](https://www.amocrm.ru/developers/content/crm_platform/companies-api),
[contacts](https://www.amocrm.ru/developers/content/crm_platform/contacts-api),
[custom fields](https://www.amocrm.ru/developers/content/crm_platform/custom-fields),
[pipelines/stages](https://www.amocrm.ru/developers/content/crm_platform/leads_pipelines),
[account identity](https://www.amocrm.ru/developers/content/crm_platform/account-info).
No live CRM credentials or real CRM API requests were used for implementation.
