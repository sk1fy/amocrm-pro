# Template variables — what is set separately

The four templates in this directory never contain live values. Render the
compose overlays by placing the variables in the corresponding `.env`
(chmod 0600, never committed); render the nginx files with `envsubst`.

Legend: **[S]** = secret (inject only), **[I]** = identifier (not a secret but
environment-specific), **[C]** = configuration (not sensitive).

## core-distribution.local.yaml  (Core worker overlay)

Put these in `/root/amocrm-pro/.env`:

| Variable | Kind | Meaning |
|---|---|---|
| `DISTRIBUTION_SERVICE_KEYS` | [S] | bridge HMAC service keys (key ring) |
| `DISTRIBUTION_TEAMOS_KEY_ID` | [I] | key id matching the service keys |
| `DISTRIBUTION_TEAMOS_URL` | [C] | TeamOS company private callback URL over the bridge |
| `DISTRIBUTION_DOMAIN` | [C] | public TLS domain (bridge cert CN) |
| `CORE_BRIDGE_GATEWAY` | [C] | docker bridge gateway the worker maps the domain to |
| `WIDGET_JWT_LEEWAY` | [C] | default `5s` (keep equal to the api) |
| `WIDGET_JWT_MAX_LIFETIME` | [C] | default `30m` (keep equal to the api) |
| `WORKER_HEALTH_PORT` | [C] | default `8081`, loopback |
| `WORKER_DISTRIBUTION_PORT` | [C] | default `18084`, loopback |

## docker-compose.staging-local.yaml  (TeamOS company stack overlay)

Put these in `/root/team-os-backend/.env`:

| Variable | Kind | Meaning |
|---|---|---|
| `COMPANY_DISTRIBUTION_SERVICE_KEYS` | [S] | TeamOS side of the bridge key ring (must match Core) |
| `COMPANY_DISTRIBUTION_KEY_ID` | [I] | key id matching the service keys |
| `COMPANY_DISTRIBUTION_CORE_URL` | [C] | Core worker private listener URL over the bridge |
| `DISTRIBUTION_DOMAIN` | [C] | public TLS domain |
| `TEAMOS_BRIDGE_GATEWAY` | [C] | docker bridge gateway the company maps the domain to |
| `POSTGRES_PORT` | [C] | default `5532`, loopback |
| `NATS_MONITOR_PORT` | [C] | default `8223`, loopback |
| `MINIO_PORT` / `MINIO_CONSOLE_PORT` | [C] | default `9100` / `9101`, loopback |
| `COMPANY_DISTRIBUTION_HTTP_PORT` | [C] | default `18081`, loopback |
| `GATEWAY_HOST_PORT` | [C] | default `8180`, loopback |

## nginx-amocrm.conf  (public TLS entrypoint)

Render with `envsubst '${DISTRIBUTION_DOMAIN} ${TLS_LIVE_DIR}'`.

| Variable | Kind | Meaning |
|---|---|---|
| `DISTRIBUTION_DOMAIN` | [C] | public hostname (server_name + cert dir) |
| `TLS_LIVE_DIR` | [C] | cert live dir, e.g. `/etc/letsencrypt/live` |

Upstream ports are fixed in the template and must match the stack:
`3000` Grafana, `18084` Core widget distribution, `8080` Core API.

## nginx-distribution-bridge.conf  (private Core<->TeamOS bridge)

Render with
`envsubst '${DISTRIBUTION_DOMAIN} ${TLS_LIVE_DIR} ${TEAMOS_BRIDGE_GATEWAY} ${CORE_BRIDGE_GATEWAY} ${TEAMOS_BRIDGE_NET} ${CORE_BRIDGE_NET} ${PUBLIC_IP}'`.

| Variable | Kind | Meaning |
|---|---|---|
| `DISTRIBUTION_DOMAIN` | [C] | public hostname (cert CN) |
| `TLS_LIVE_DIR` | [C] | cert live dir |
| `TEAMOS_BRIDGE_GATEWAY` | [C] | docker bridge gateway TeamOS listens on (e.g. `172.24.0.1`) |
| `CORE_BRIDGE_GATEWAY` | [C] | docker bridge gateway Core listens on (e.g. `172.18.0.1`) |
| `TEAMOS_BRIDGE_NET` | [C] | allowed source CIDR, e.g. `172.24.0.0/16` |
| `CORE_BRIDGE_NET` | [C] | allowed source CIDR, e.g. `172.18.0.0/16` |
| `PUBLIC_IP` | [C] | host public IP explicitly allowed by the ACL |

Upstream ports are fixed: `18084` (Core widget distribution) and `18081`
(TeamOS company callback). Keep the `deny all` + explicit `allow` ACL and the
loopback-only bindings; do not widen to `0.0.0.0`.

## Values that must be generated, not copied

- `DISTRIBUTION_SERVICE_KEYS` / `COMPANY_DISTRIBUTION_SERVICE_KEYS`: generate a
  fresh shared key ring per environment; the Core and TeamOS sides must match
  key-for-key (same key ids). Never reuse production keys on a test host.
- TLS material: issue your own certificate for `DISTRIBUTION_DOMAIN`; do not
  copy the staging `privkey.pem`.
