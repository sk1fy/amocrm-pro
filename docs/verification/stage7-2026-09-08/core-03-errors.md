# CORE-03. Inventory of public JSON/HTTP errors

Date: 10 September 2026. HEAD at start of this slice: `dac12af`. Public surface is `cmd/api` as documented by `api/openapi.yaml`.

`incomplete` is not an HTTP error code. It is a data/coverage state on Activity panel (`coverage`, freshness vs verified range). It stays distinct from `unavailable`, `not_found` and empty history.

## Envelope families

| Family | Shape | Codes / text | request_id | retryable | Used by |
| --- | --- | --- | --- | --- | --- |
| Activity JSON | `{"error":{"code","message","request_id","retryable"}}` | `invalid_argument`, `unauthenticated`, `permission_denied`, `not_found`, `conflict`, `unavailable`, `deadline_exceeded`, `resource_exhausted`, `reauth_required`, `internal` | `X-Request-ID` | true only for unavailable / resource_exhausted / deadline_exceeded | `internal/activitybridge/http.go` `writeError` |
| OAuth JSON | same fields | `invalid_argument`, `unavailable`, `rate_limited` | `X-Request-ID` | true for unavailable and rate_limited | OAuth Start/Callback errors and `internal/oauthlimit` 429 |
| Widget limiter JSON | `{"error":{"code":"rate_limited"}}` | `rate_limited` | no | no | `internal/widgetlimit` on every widget/Activity/lead-status route |
| Widget capability JSON | `{"error":{"code":"service_not_enabled"}}` | `service_not_enabled` | no | no | widget ping, lead-status commands |
| Job status JSON | success body may include `error.code` | worker `LastErrorCode` | n/a | n/a | `GET /api/v1/widget/jobs/{jobID}` |
| text/plain | `http.Error` generic phrase | see below | header only | no | webhook, widget auth/CORS, lead-status, recover |

## HTTP mapping kept distinct

| Semantic | JSON `code` | HTTP | Notes |
| --- | --- | --- | --- |
| unauthenticated | `unauthenticated` | 401 + `WWW-Authenticate: Bearer` | Widget JWT missing/invalid/replay. Activity `writeError` and widget auth middleware (middleware is text/plain). |
| denied | `permission_denied` | 403 | Capability, pilot, admin role. No Bearer challenge. |
| reauth_required | `reauth_required` | **403** | Installation needs amoCRM reauthorization. Distinct from 401 JWT and from 503. No `WWW-Authenticate`. Widget JWT already succeeded. |
| unavailable | `unavailable` | 503 (Activity), 502 (OAuth callback upstream) | Not empty history. OAuth start does **not** use 503 so missing vs other start failures stay indistinguishable. |
| rate_limited | `rate_limited` | 429 + `Retry-After` | Widget limiter and OAuth ingress limiter. |
| resource_exhausted | `resource_exhausted` | 429 + `Retry-After: 1` | Activity downstream/capacity via `serviceapi.ResourceExhausted`. Distinct from ingress `rate_limited`. |
| incomplete | (not an error code) | 200 with coverage/freshness | Must not be mapped to 404/503/empty page. |
| invalid_argument | `invalid_argument` | 400 | |
| not_found | `not_found` | 404 | |
| conflict | `conflict` | 409 | |
| deadline_exceeded | `deadline_exceeded` | 504 | retryable |

`reauth_required` previously fell through Activity `writeError` to 503. That collapsed it into `unavailable`. It is now 403. Documented here instead of a separate ADR: the widget session is authenticated; the installation credential is not.

Safe `message` values are generic per code. `serviceapi.Error.Message` is not copied, so upstream/database text cannot leak.

## Path inventory

### OAuth `/oauth/amocrm/start`, `/oauth/amocrm/callback`

| Outcome | HTTP | Body | Leak |
| --- | --- | --- | --- |
| Start success | 302 Location | empty | n/a |
| Start missing integration, invalid return_url, oversized query, other start failure | 400 | JSON `invalid_argument` / `cannot start authorization` | same body for known and unknown `integration_code` |
| Callback success | 201 | `InstallationResult` | |
| Callback denied (`error=`), invalid/used state, oversized state/code/referer | 400 | JSON `invalid_argument` / `authorization failed` | no amoCRM error string |
| Callback token/account failure | 502 | JSON `unavailable` / `authorization failed` | no upstream detail |
| Either path over limiter | 429 | JSON `rate_limited` / `rate limited` | identical for known vs unknown code/state |

Previously Start/Callback used `http.Error` text/plain (`cannot start authorization`, `authorization denied`, `authorization failed`) with no envelope. Callback user-deny text was folded into `authorization failed`.

### Webhook `POST /hooks/amocrm/v1/{webhookKey}` — inventory only

text/plain. No JSON envelope. Outcomes unchanged: 204 durable accept (including invalid-payload audit), 400, 404, 413, 415, 429 `rate limit exceeded` + `Retry-After`, 503 `temporarily unavailable`. 404 is used for unknown key and account mismatch (non-enumeration of live tenants is existing policy). Not a contract bug for CORE-03; changing the body would not change inbox/job outcomes and was out of scope.

### Widget bootstrap/ping/job

Mostly text/plain (`unauthorized`, `invalid idempotency key`, `idempotency conflict`, `temporarily unavailable`). Ping 403 JSON `service_not_enabled`. Limiter 429 JSON `rate_limited` only. Inventory only.

### Lead-status URLs — inventory only

`POST /api/v1/widget/actions/leads/set-status` and `POST /api/v1/widget/workflow-rules/lead-status/configure` keep the same URLs, 202 + idempotency replay header, 400/401/409/503 text, 403 JSON `service_not_enabled`, widget 429 `rate_limited`. No handler change.

### Activity widget routes

After JWT/CORS/widget limiter, `writeError` now emits the Activity JSON envelope. Middleware failures in front of the handler remain text/plain 401/403/429-without-message. That split already existed and is noted in OpenAPI (`Existing widget JWT/CORS/ingress limits apply; middleware errors may use the existing text/plain authentication contract`).

## Remaining mismatches (not changed)

- Widget limiter 429 still `{code:rate_limited}` without message/request_id/retryable.
- Webhook and lead-status still text/plain except `service_not_enabled`.
- Recover/method-not-allowed/internal auth storage failures remain text/plain 500/405.
- Activity 429 from the handler is `resource_exhausted`; 429 from widget limiter is `rate_limited`. Clients must read `code`, not only HTTP status.
- `request_id` in JSON is the `X-Request-ID` header set by middleware; tests that call `writeError` directly must set the header.
