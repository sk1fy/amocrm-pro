# Private amoCRM widget E2E

- **Status:** Blocked by `BUG-010` until `X-Auth-Token` support is implemented.
- **Scope:** Development verification of one private integration and account.

This runbook describes the intended end-to-end path. It must not be presented
as passing evidence until a real installed widget completes every verification
step below.

## Preconditions

1. A private amoCRM integration with a JS widget is created in the target test
   account.
2. API is reachable through a public HTTPS origin. `localhost` is not usable
   from an amoCRM account browser page.
3. Redirect URI is exactly:

   ```text
   https://amo-backend.example.test/oauth/amocrm/callback
   ```

4. API, worker and PostgreSQL use the same encryption keyring configuration.
5. Only synthetic/test account data is used.

amoCRM documents disposable integration tokens and `this.$authorizedAjax()` at:

- https://www.amocrm.ru/developers/content/oauth/disposable-tokens
- https://www.amocrm.ru/developers/content/web_sdk/mechanics

## Backend configuration

Create an untracked `.env` from `.env.example` and set at minimum:

```dotenv
APP_ENV=development
PUBLIC_BASE_URL=https://amo-backend.example.test
BOOTSTRAP_INTEGRATION_CODE=my_private_widget
AMOCRM_CLIENT_ID=<private-integration-uuid>
AMOCRM_CLIENT_SECRET=<private-integration-secret>
AMOCRM_REDIRECT_URI=https://amo-backend.example.test/oauth/amocrm/callback
ENCRYPTION_KEYS=1:<base64-encoded-32-byte-key>
ACTIVE_ENCRYPTION_KEY_VERSION=1
```

Do not reuse the development example encryption key for a shared or production
environment. Do not paste any real value into Issues, logs or documentation.

Validate and start:

```sh
make config
make build
make up
make ps
```

Confirm API and worker health before OAuth.

## OAuth installation

Open in a browser:

```text
https://amo-backend.example.test/oauth/amocrm/start?integration_code=my_private_widget
```

Complete authorization in the intended amoCRM account. A successful callback
returns `201` with an installation ID/account ID and commits a webhook reconcile
job.

If the widget was installed before this backend was configured, its installation
alone does not create this service's PostgreSQL installation record. Run the
stateful `/oauth/amocrm/start` flow explicitly.

Verify only redacted state:

- installation is active;
- encrypted credential rows exist;
- reconcile job reaches a terminal successful state;
- no raw token or secret appears in logs.

## Widget bootstrap

After `BUG-010` is fixed, call the backend only through Web SDK so amoCRM creates
a fresh disposable JWT:

```js
self.$authorizedAjax({
  url: 'https://amo-backend.example.test/api/v1/widget/bootstrap',
  method: 'GET',
  dataType: 'json'
}).done(function (response) {
  console.log('widget bootstrap', response);
}).fail(function (xhr) {
  console.error('widget bootstrap failed', xhr.status);
});
```

Expected: `200` with `installation_id`, `account_id`, `user_id` and
`client_uuid`. The response must describe the current test account/user.

Every request needs a fresh disposable JWT. Do not reuse a completed
`$authorizedAjax()` token/request.

## Queue smoke test

```js
self.$authorizedAjax({
  url: 'https://amo-backend.example.test/api/v1/widget/actions/ping',
  method: 'POST',
  headers: {
    'Idempotency-Key': crypto.randomUUID()
  }
}).done(function (response) {
  console.log('job accepted', response);
});
```

Expected: `202` and `job_id`. Use a new `$authorizedAjax()` call to read:

```text
GET /api/v1/widget/jobs/{job_id}
```

Expected terminal result:

```json
{"pong": true}
```

## Lead-status workflow

Run only as an active amoCRM administrator and only against a synthetic lead:

```http
POST /api/v1/widget/actions/leads/set-status
Content-Type: application/json
Idempotency-Key: <new-random-value>

{"lead_id": 1, "pipeline_id": 2, "status_id": 3}
```

Verify:

- admission returns `202`;
- worker checks the current amoCRM user and lead;
- exactly one PATCH occurs when state differs;
- retry/status polling does not repeat the external effect;
- public job result contains only the typed redacted outcome.

## Failure checks

The E2E is incomplete unless these scenarios are observed:

- replaying one disposable token returns `401`;
- wrong account origin is rejected by CORS;
- inactive installation cannot enqueue or execute an action;
- non-admin user cannot execute the privileged lead-status mutation;
- `401` from amoCRM triggers at most one refresh/retry;
- no token, secret, webhook key or raw production payload appears in logs.

## Cleanup

Stop the development stack with `make down`. If this was an isolated disposable
environment and its database is intentionally no longer needed, `make destroy`
removes the local Compose volume. Do not run `make destroy` against an environment
whose data must be retained.
