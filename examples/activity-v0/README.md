# Activity v0 adapter for an existing widget

`panel.mjs` and `panel.css` mount a small Activity panel in the existing widget's
chosen container. They do not create an integration, change another widget, or
contain installation/account/actor credentials. Load the CSS within the existing
widget lifecycle and call `destroy()` when that container is removed.

The SDK contract was checked on 2026-09-06 against the official
[amoCRM Web SDK mechanics](https://www.amocrm.ru/developers/content/web_sdk/mechanics):
`widget.$authorizedAjax(options)` accepts jQuery AJAX options, supplies the
temporary `X-Auth-Token` itself, and returns a jQuery-compatible Deferred.
The bridge below uses its documented `.done/.fail` callbacks. Every request,
including each status check and an explicit command retry, calls the SDK again.

```js
// Inside your existing widget, where widget is the Widget instance and
// container is an element rendered by that widget. Use your deployed HTTPS Core
// origin allowed by the integration's existing tenant-bound CORS setup.
const { mountActivityPanel } = await import(widget.params.path + '/activity-v0/panel.mjs');
const backend = 'https://your-core.example';
const authorizedRequest = ({ path, method, body, headers }) => new Promise((resolve) => {
  widget.$authorizedAjax({
    url: backend + path,
    method,
    dataType: 'json',
    contentType: 'application/json',
    processData: false,
    timeout: 15000,
    headers,
    ...(body === undefined ? {} : { data: JSON.stringify(body) }),
  }).done((data, _textStatus, xhr) => {
    resolve({ status: xhr.status, body: data });
  }).fail((xhr) => {
    resolve({ status: xhr.status || 0, body: xhr.responseJSON || {} });
  });
});
const activityPanel = mountActivityPanel(container, authorizedRequest);
// In your existing destroy callback:
// activityPanel.destroy();
```

The mount expects `authorizedRequest({path, method, body, headers})` to return a
Promise of `{status, body}`. It never accepts a token argument or stores tokens.
Command identity is retained in memory during a failed request; the explicit
retry uses the same body and Idempotency-Key. Do not reload the panel to recover
an ambiguous command without first reconciling the Core receipt. Accepted
operations get at most 12 status checks, one every five seconds, followed by
manual checks. There is no automatic mutation retry or continuous history poll.

All v0 endpoints require server-confirmed amoCRM administrator rights plus
integration `activity` capability and an operator-enabled installation pilot.
“Приостановить сбор” pauses the consumer; it does not revoke access to retained
history. Product disable is the Core operator pilot control. Settings take effect
in CRM Events on the next sync command, whose receipt snapshots the settings.

Filters are limited to 31 days, 100 employees and 100 events per page. Counts
refer to registered CRM events; unknown/partial/stale coverage is shown beside
the data and is never called proof of employee inactivity. The browser chooses
date inputs in its own local time, while event timestamps use account timezone.

Run the pure presentation/bounds checks with:

```sh
node --test examples/activity-v0/panel.test.mjs
```

Operation state has one success value: `succeeded`. `accepted`, `running`,
`retry`, `paused`, and `failed` cover the remaining owner states; Core additionally
reports `pending_delivery` before delivery. The CRM Events application maps its
existing stored `completed` value to `succeeded`, including replay and restart.
Existing Core jobs retain their separate `completed` contract.

The mounted-panel regression consumes a response emitted by a real CRM Events
PostgreSQL test. Run the commands in an environment with Go and Node available,
or run the first command in the existing Docker test image with the output path
mounted for the second command. `CRM_EVENTS_TEST_DATABASE_URL` must select an
isolated database ending in `_test`; that test resets the owner tables.

```sh
ACTIVITY_UI_OPERATION_FIXTURE=/tmp/activity-operation.json \
  go test -count=1 -run '^TestOperationSuccessContractPreservesCompletedStorage$' \
  ./internal/services/crmevents
ACTIVITY_UI_OPERATION_FIXTURE=/tmp/activity-operation.json \
  node --test examples/activity-v0/panel.test.mjs examples/activity-v0/panel.operation.test.mjs
```

The test drives the actual mounted panel with a small DOM and controlled timers.
It verifies both scheduled and manual status checks: the returned `succeeded`
state displays completion, cancels further polling, and refreshes exactly once.
Without the generated response path, these two receiver-dependent Node cases
are explicitly skipped; their execution is not inferred from presentation tests.

This is a reusable adapter, not a submitted marketplace archive. The real
installed-widget browser flow, the target widget's asset loading/CSP, admin
rights, CORS and a new live amoCRM event must still be checked in the pilot.
The Node tests do not claim that integration has been performed.
