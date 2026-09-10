# CORE-03. HTTP contracts and admission

Date: 10 September 2026. Plan: `docs/plans/2026-09-08-activity-backend-development.md` stage 7. Error inventory: [core-03-errors.md](core-03-errors.md).

Existing base: Activity typed errors and query/body bounds, widget limiter on widget routes, webhook ingress limiter and payload cap. Confirmed gap: OAuth Start/Callback were registered with no process limiter; widget middleware does not cover them.

## What changed vs already existed

| Item | Before | CORE-03 |
| --- | --- | --- |
| OAuth Start/Callback limiter | none in API wiring | `internal/oauthlimit` around both routes only |
| OAuth errors | text/plain | JSON envelope, generic messages, no existence leak |
| Activity `writeError` | `{error:{code}}`; `reauth_required` → 503 | `code` + safe `message` + `request_id` + `retryable`; `reauth_required` → 403 |
| OAuth query bounds | unbounded strings (HTTP header cap only) | integration_code 128, return_url 1024, state 256, code 2048, referer 256 |
| Activity body/query/cursor/timeout | already bounded | no change (proved below) |
| Webhook/lead-status handlers, jobs, idempotency | already bounded + durable | inventory only |
| Reverse proxy / target server | unknown | **not verified** |

Lead-status URLs, job types and idempotency outcomes were not modified.

## OAuth limiter

Process-local token buckets, same pattern as `widgetlimit`, **not** mounted on the widget limiter (no JWT, no verified tenant).

Keys (SHA-256, never raw IP/code/state, never as metric labels):

- `ip` + `RemoteAddr` host. `X-Forwarded-For` is ignored (untrusted without a verified proxy hop).
- `identity`: Start hashes `c\\0` + clipped `integration_code` (128); Callback hashes `s\\0` + clipped `state` (256). Empty values share one bucket.

Both budgets are checked under one mutex; a rejection spends neither. Full cache (`MaxEntries`) rejects new keys after idle eviction, without resetting active buckets.

429 body is identical for known and unknown `integration_code` / `state`:

```json
{"error":{"code":"rate_limited","message":"rate limited","request_id":"<X-Request-ID>","retryable":true}}
```

plus `Retry-After` and `Cache-Control: no-store`.

Metrics: `amocrm_oauth_limit_decisions_total{scope,outcome}` and `amocrm_oauth_limit_entries{scope}` with `scope` in `{ip,identity,capacity,tenant}` and `outcome` in `{allowed,rejected}`.

Config (end of `config.API`, widget-style env, validated like widget TTL/burst):

| Env | Default |
| --- | --- |
| `OAUTH_IP_RATE_PER_SECOND` | 5 |
| `OAUTH_IP_BURST` | 20 |
| `OAUTH_IDENTITY_RATE_PER_SECOND` | 2 |
| `OAUTH_IDENTITY_BURST` | 8 |
| `OAUTH_LIMITER_INACTIVE_TTL` | 10m (must cover a full burst refill, ≥1s) |
| `OAUTH_LIMITER_MAX_ENTRIES` | 10000 |

This is per API process. It does not replace an edge reverse-proxy limit.

## Bounds

| Path | Body | Lists / cursor | Timeout | Concurrent reads |
| --- | --- | --- | --- | --- |
| Activity panel/query | n/a GET | `ValidateQuery`: 31d, ≤100 users, cursor ≤512, limit 1..100, ≤32 types, ≤100 entity_ids, ≤11 categories; `decode` POST 4096 bytes, unknown fields rejected | 10s per handler; HTTP ReadTimeout 10s / WriteTimeout 15s | widget limiter; one RPC per request, no unbounded fan-out. **No new process semaphore.** |
| Activity commands | 4096 JSON object | idempotency key validated | 10s | widget limiter |
| Widget ping | empty, MaxBytesReader 1 | n/a | server timeouts | widget limiter |
| Lead-status | 1024 / 2048 | n/a | server timeouts | widget limiter |
| Webhook | `MAX_WEBHOOK_BODY_BYTES` default 2MiB (1KiB..16MiB), `WEBHOOK_TIMEOUT` <2s | n/a | handler deadline | global + installation ingress limiter |
| OAuth | GET only | lengths above | `AMOCRM_REQUEST_TIMEOUT` on exchange; HTTP timeouts | **new** oauthlimit |

## OpenAPI

`api/openapi.yaml` updated with the implementation:

- OAuth 400/502 JSON (`OAuthError` / `OAuthUnavailable`), 429 `OAuthRateLimited`.
- Parameter maxLengths for OAuth query fields.
- ActivityError fields `message`, `request_id`, `retryable`; 403 text mentions `reauth_required`.
- Lead-status paths unchanged.

`go test ./api` validates the document.

## Tests

```sh
gofmt -w cmd/api/main.go internal/oauth/handler.go internal/oauth/handler_test.go \
  internal/oauthlimit/limiter.go internal/oauthlimit/limiter_test.go \
  internal/activitybridge/http.go internal/activitybridge/http_test.go \
  internal/platform/config/config.go
go test -c -o /tmp/oauthlimit.test ./internal/oauthlimit
go test -c -o /tmp/oauth.test ./internal/oauth
go test -c -o /tmp/activitybridge.test ./internal/activitybridge
go test -c -o /tmp/apiopenapi.test ./api
go test ./internal/oauthlimit ./internal/oauth ./internal/activitybridge ./api ./internal/platform/config -count=1 -timeout 60s
go test -c -o /tmp/api.bin ./cmd/api
go vet ./internal/oauthlimit ./internal/oauth ./internal/activitybridge ./cmd/api ./internal/platform/config
```

Host results 10 September 2026: compile exit 0; all listed `go test` packages PASS; `go vet` PASS.

Covered behaviour:

- OAuth limiter 429 for Start and Callback; known vs unknown code/state produce the same body.
- Unlimited Start still 302; unlimited Callback still 201.
- Start missing vs other failure: both 400 `invalid_argument` `cannot start authorization`.
- Activity `writeError` includes `code` + `request_id`; `reauth_required` is 403 not 503; `sensitive upstream detail` is not leaked. Existing `TestStableErrorMapping` updated, not deleted.

## Residuals

- Production reverse proxy / target-server ingress was **not** verified. The process limiter is not a cluster-wide or edge substitute.
- Widget limiter and webhook/lead-status text/plain envelopes were left as-is (inventory).
- `OAUTH_*` env keys are loaded in `config.LoadAPI`; dedicated LoadAPI unit cases were not added (limiter.New still rejects unsafe values at process start).
- `make activity-ci`, Docker Postgres suites and live amoCRM were not run (coordinator-owned).
