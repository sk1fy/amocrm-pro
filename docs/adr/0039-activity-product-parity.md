# ADR 0039: Activity 2.0 product data and browser administration

Status: local implementation, 2026-10-26. Telegram is deferred.

- Core Gateway owns the current CRM snapshot: one bounded v4 page per durable
  generation/phase/page checkpoint. The complete generation is published only
  after leads and active tasks finish. No maximum account size. Old generation
  remains readable with timestamp/stale/error during refresh. Worker shares the
  existing amoCRM client, token provider and request budget, and checks current
  installation/pilot/capability admission before every batch. Activity never
  reads Core tables; scoped counts cross an explicit RPC port.
- Activity owns panel options, global task-panel settings and observed work /
  activity / pause intervals. These are not inferred from CRM events. Starts
  serialize per installation/integration/employee and atomically stop the old
  interval. Stop needs the acknowledged session ID. Command UUID + canonical
  payload hash provides replay without duplicating observations. Server times
  are authoritative; history reads paginate by timestamp/UUID with scoped cursors.
- An employee delegation is issued only by Core after fresh widget JWT and a
  current active-user check. It grants only Activity sessions, with actor from
  the verified JWT. Enabling collection for that actor is checked by Activity.
  Viewer keys cannot ingest sessions or manage settings.
- Browser management uses a random 256-bit opaque credential, stored only as a
  SHA256 hash in Core, scoped to installation/integration/admin actor, expiring
  in 30 minutes. Issuance is limited to a configured application origin and a
  freshly verified admin. Every management request reissues a 30-second internal
  delegation with a fresh admin-role check; the internal JWT and static management
  token never leave Core. Browser credential is delivered in a URL fragment,
  removed by the frontend and kept only in tab memory. Reload needs another entry
  from the widget. Existing static management access remains for server tools.
- New history settings default to 31/31 days. Existing settings are not silently
  rewritten; widening them requires configure/backfill. Monthly analytics is not
  implemented by this change. Downgrade refuses incompatible longer settings.

The new widget now selects a live controller in amoCRM and uses the SDK's
`$authorizedAjax` for employee state, sessions and administrative entry. Real task
reads/completion use same-origin CRM APIs; demo data stays in the preview controller.
Imbox DOM selectors and card hooks still need validation against a controlled live
account. Related tasks are annotated in the combined queue. The task watchdog runs
in both process topologies; closed session history retains32days. Command receipts
are retained for replay safety until a separate expiry/tombstone policy exists.

CRM read methods use the official v4 [leads API](https://www.amocrm.ru/developers/content/crm_platform/leads-api)
and [tasks API](https://www.amocrm.ru/developers/content/crm_platform/tasks-api).
Legacy private HTML-count filters are not copied into the new credentialed client.
Their semantics still require live comparison on the same controlled account.
