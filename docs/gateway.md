# The Centauri gateway — an API gateway with a perfect memory

`centauri gateway` turns a Centauri log into a reverse proxy whose **entire
control plane lives in the database**. Routes and API keys are ordinary facts;
every config change is a superseding fact, never an overwrite — so your gateway
configuration **time-travels**:

```
FACTS OF route:api FACET config AS OF 'last tuesday 9am'   -- what was live then
FACTS OF route:api FACET config AS KNOWN AT '2026-08-01'   -- what we believed then
HISTORY OF route:api                                        -- every version ever
```

And the request audit is written to the same append-only, hash-chained log, so
`centauri verify -data gateway.log` proves nobody edited the traffic record
after the fact. That is the pitch in one line: **config you can time-travel,
an audit you can prove.** (For what Kong still does better, see the
[honest comparison](#centauri-gateway-vs-kong-honestly) below.)

The data plane is Go's standard-library reverse proxy — zero third-party
dependencies, one binary, same as the rest of Centauri.

## Quickstart

```
# 1. Start it. Data plane on :8080, admin plane on :7771.
centauri gateway -data gateway.log -addr :8080 -admin :7771
```

The admin plane is a normal Centauri server over `gateway.log`: open
**http://localhost:7771** and you get the full dashboard (the home screen grows
a **Gateway** card), query bar, and API. Add `-token …` to require a bearer
token on the admin plane.

```
# 2. Add a route — a plain CeQL PUT, from the dashboard query bar or curl:
PUT route:api FACET config SET prefix='/api/', upstream='http://127.0.0.1:9000', auth='key', rate_per_min=120, timeout_ms=10000, audit='meta'
```

```
# 3. Mint an API key. The key is printed ONCE; only its sha256 is stored.
centauri gateway -data gateway.log -newkey ci-bot
```

Prefer the browser? The **Console** (`/console` → **Keys & Access**) mints keys
live over `POST /v1/gateway/keys` — no need to stop the gateway. `-newkey` stays
as the offline/CLI path; it takes the writer lock, so run it before starting the
gateway (or stop it first).

```
# 4. Call through the gateway (the key travels as a bearer token):
curl -H 'Authorization: Bearer <the printed key>' http://localhost:8080/api/health
```

Updating a route is just another `PUT route:api FACET config SET …` — the old
version is superseded, kept forever, and visible under `AS OF`. Turning a route
off is `RETIRE route:api FACET config` (no DELETE exists; history stays).

## Route facts, field by field

A route is the current fact of subject `route:<name>`, facet `config`:

| field          | type   | meaning                                                                                         |
|----------------|--------|-------------------------------------------------------------------------------------------------|
| `prefix`       | string | Requests whose path starts with this prefix use this route, e.g. `/api/`.                       |
| `upstream`     | string | Base URL the request is proxied to (non-LLM routes).                                            |
| `strip_prefix` | bool   | Remove `prefix` from the path before forwarding.                                                 |
| `auth`         | string | `"none"` (open), `"key"` (minted API key), or `"oidc"` (validated JWT — see below). Anything else rejects the route: a typo can never mean "open". |
| `rate_per_min` | number | Requests allowed per minute on this route; absent/0 = unlimited.                                 |
| `timeout_ms`   | number | Upstream timeout in milliseconds.                                                                |
| `audit`        | string | `"meta"` writes one audit fact per request (metadata only, never bodies); `"none"` writes nothing. |
| `llm`          | bool   | Treat as an OpenAI-compatible LLM route with provider failover (see below).                      |
| `providers`    | array  | LLM only: `{endpoint, model, auth_env, auth_file}` entries, tried in order.                      |
| `oidc_issuer`  | string | `auth:"oidc"`: expected `iss`; also used for JWKS discovery. Required (with audience) on OIDC routes. |
| `oidc_audience`| string | `auth:"oidc"`: the `aud` the token must carry. Required on OIDC routes.                          |
| `oidc_jwks_url`| string | `auth:"oidc"`: explicit JWKS endpoint when the issuer has no discovery document. Optional.       |
| `add_request_headers` | map | Middleware: headers set on the forwarded request.                                            |
| `add_response_headers`| map | Middleware: headers set on the response.                                                     |
| `remove_request_headers` | array | Middleware: headers deleted before forwarding.                                            |
| `cors`         | bool   | Middleware: permissive CORS defaults + OPTIONS preflight short-circuit (before auth).            |
| `allow_ips`    | array  | Middleware: CIDRs (bare IPs fine) that may call; anything else is 403.                           |
| `deny_ips`     | array  | Middleware: CIDRs refused with 403. Deny wins over allow.                                        |
| `max_body_bytes` | number | Middleware: 413 for larger request bodies.                                                     |
| `block_path_patterns` | array | Middleware: `path.Match` globs answered 403 (`*` does not cross `/`).                       |
| `policy`       | string | Name of a stored CePL procedure run per request — allow/deny/add headers (see [Request policies](#request-policies-cepl--plugins-the-centauri-way)). A missing or unparsable procedure rejects the whole route. |
| `authz_webhook`| string | URL POSTed per request after auth; anything but a 2xx answer is 403 (see [Webhook authorization](#webhook-authorization)). |
| `webhook_fail_open` | bool | Allow requests when the webhook is unreachable/non-2xx (default: fail closed).             |
| `retired`      | bool   | Set by `RETIRE` — the route stops matching; its history remains queryable.                       |

Maps and arrays (like `providers`) go through the raw `/v1/append` endpoint or
an SDK — CeQL `SET` takes scalars only.

## Key facts

A key is the current fact of subject `gwkey:<sha256-of-key>`, facet `config`.
Mint one live in the **Console** (`/console` → Keys & Access) while the gateway
runs, or offline with `-newkey <name>`. **The plaintext key is shown once and
never stored** — the log holds only the hash, so a leaked log file does not
leak keys.

| field          | type   | meaning                                                                 |
|----------------|--------|-------------------------------------------------------------------------|
| `name`         | string | Human label (`ci-bot`), and what audit facts record as `key`.           |
| `routes`       | array  | Route prefixes this key may call; empty = all routes.                   |
| `rate_per_min` | number | Per-key budget, on top of any per-route limit.                          |
| `retired`      | bool   | `RETIRE gwkey:<sha256> FACET config` revokes the key; history stays.    |

## LLM gateway recipe

Set `llm: true` and give a `providers` list; the gateway speaks the
OpenAI-compatible chat-completions shape and **fails over in order**: if a
provider is unreachable or returns a 5xx, the next one is tried. Each provider
entry **pins the model** — the gateway forces its `model` into the forwarded
request, so callers cannot pick an arbitrary (expensive) model. Secrets never
live in facts: `auth_env` names an environment variable holding that provider's
API key, `auth_file` a path to a key file — use one or neither (local servers
like Ollama are keyless).

`providers` is a nested array, and CeQL `SET` takes scalar values only — so
write LLM routes through the raw append endpoint (or any SDK) instead of `PUT`:

```
curl -s http://localhost:7771/v1/append -H 'Content-Type: application/json' -d '{
  "events": [{
    "subject": "route:chat", "facet": "config", "type": "OBSERVED",
    "value": {
      "prefix": "/v1/chat/", "llm": true, "auth": "key", "audit": "meta",
      "providers": [
        {"endpoint": "http://127.0.0.1:11434/v1/chat/completions", "model": "glm-4.7-flash"},
        {"endpoint": "https://api.openai.com/v1/chat/completions", "model": "gpt-5.5", "auth_env": "OPENAI_API_KEY"}
      ]
    }
  }]
}'
```

When a provider reports usage, the audit fact records `model`,
`prompt_tokens`, and `completion_tokens` — your per-key token spend is a query,
not a billing-dashboard export.

**Streaming:** `stream: true` is passed through as SSE — chunks are flushed
to the caller as the provider sends them. The gateway injects
`stream_options.include_usage` into the forwarded request so the trailing
usage chunk still lands token counts in the audit fact (your client receives
that extra chunk too; when a provider sends no usage, the request is audited
without token counts). Provider failover applies **until the first byte**:
once streaming has started, a provider failure ends the stream early —
truncated but properly terminated, with `truncated: true` in the audit fact.
That is the honest physics of SSE, not a gateway shortcut.

## OIDC-protected routes

Set `auth: 'oidc'` plus `oidc_issuer` **and** `oidc_audience` (both required —
a half-configured OIDC route refuses to match rather than serve open):

```
PUT route:api FACET config SET prefix='/api/', upstream='http://127.0.0.1:9000', auth='oidc', oidc_issuer='https://login.example.com', oidc_audience='my-api'
```

The gateway validates the caller's `Authorization: Bearer <JWT>` with the same
stdlib-only verifier `centauri serve` uses for SSO: RS256/384/512 signature
against the issuer's JWKS (fetched once per config load and cached, refreshed
on key rotation), plus expiry, not-before, issuer, and audience checks.
Failures get a JSON 401. On success the request is forwarded with
`X-Auth-Subject: <sub>` — any client-supplied value of that header is stripped
on every route, so upstreams can trust it — and the audit fact's `key` field
is the JWT `sub`.

**The bearer token is passed through to the upstream** (upstreams often
validate it themselves). If yours must not see it, add
`"remove_request_headers": ["Authorization"]` to the route — middleware runs
after auth, so validation still happens.

Honesty note: validating your IdP's tokens does not make the gateway an IdP —
no login flows or session management here. It *can*, however, mint short-lived
**service** tokens for its own keys (client-credentials only) that these same
OIDC routes accept — see
[Issuing tokens](#issuing-tokens-client-credentials--an-idp-lite). Human
sign-in stays your IdP's job.

## Issuing tokens (client-credentials) — an IdP-lite

The gateway can also **issue** short-lived service tokens, so
service-to-service callers can pass `auth: 'oidc'` routes without an external
identity provider. Honestly scoped: **OAuth2 `client_credentials` only** —
no human login, no consent screens, no refresh tokens, no sessions. This is
for machines; bring your IdP for people.

Every gateway key doubles as an OAuth2 client: `client_id` is the key's
**name**, `client_secret` is the key itself (RFC 6749 §4.4, form-encoded):

```
curl -s http://localhost:8080/gw/token \
  -d grant_type=client_credentials \
  -d client_id=ci-bot \
  -d client_secret=<the minted key>
# -> {"access_token":"eyJ…","token_type":"Bearer","expires_in":900}
```

The token is an RS256 JWT — `iss` = `-issuer`, `sub` = the key name, `aud` =
`-token-aud`, `exp` after `-token-ttl` seconds, random `jti` — signed with an
RSA-2048 key generated on first use and stored as PEM **next to** the log
(`<data-dir>/issuer.key`, mode 0600), never *in* it: the log is append-only
forever, so a secret that entered it could never be erased. The public half
is served from the proxy listener:

- `GET /.well-known/jwks.json` — the key set (RFC 7517);
- `GET /.well-known/openid-configuration` — minimal discovery
  (`issuer`, `token_endpoint`, `jwks_uri`).

Self-validation closes the loop — a route can require the gateway's own
tokens:

```
PUT route:svc FACET config SET prefix='/svc/', upstream='http://127.0.0.1:9100', auth='oidc', oidc_issuer='http://localhost:8080', oidc_audience='centauri', oidc_jwks_url='http://localhost:8080/.well-known/jwks.json'
```

Every issuance — and every failed attempt — is audited on `gw:_tokens`
(`{sub, ok}`, never any secret material). `RETIRE gwkey:…` stops **new**
tokens immediately; already-issued tokens live out their TTL, which is short
(900s) by design.

## mTLS on the proxy listener

```
centauri gateway -data gateway.log -tls-cert srv.pem -tls-key srv.key -mtls-ca clients-ca.pem
```

With `-mtls-ca <pem>`, the proxy listener requires every connection to present
a client certificate signed by that CA (`RequireAndVerifyClientCert`) —
requests without one die at the TLS handshake, before any route logic.
`-tls-cert`/`-tls-key` are required alongside it. When key/OIDC auth hasn't
named the caller, audit facts record the client certificate's CN as `key`.
This is listener-wide, not per-route.

## Built-in middleware — not a plugin ecosystem

Honest naming up front: this is a **small fixed set** of per-route tweaks, not
Kong's plugin ecosystem — there is no third-party plugin API. What exists
(fields in the route table above): request/response header add + request
header remove, permissive CORS with preflight short-circuit, IP allow/deny
lists, request body caps, and blocked path globs. For *programmable* per-route
logic, see the next two sections: CePL request policies and webhook
authorization callouts.

Order per request: CORS (before auth — browsers send preflights without
credentials) → auth → rate limit → header add/remove → IP filters → body cap
→ path blocks → authorization webhook → CePL policy → proxy. Parsing is
fail-closed: a route with an unparsable CIDR, a bad glob, an unknown `auth`
value, a broken webhook URL, or a missing policy procedure is rejected whole
(its paths 404) — a typo can never fail open. And because routes are facts,
middleware config hot-reloads and time-travels like everything else.

## Request policies (CePL) — plugins, the Centauri way

Instead of a plugin API, a route can name a **stored CePL procedure** that
runs on every request — after auth, rate limiting, and the built-in
middleware, immediately before proxying:

```
PUT route:api FACET config SET prefix='/api/', upstream='http://127.0.0.1:9000', auth='key', policy='office_rules'
```

The policy is a normal stored procedure (subject `proc:office_rules`), so it
is versioned, hot-reloaded on change, and `AS OF`-auditable like every other
fact — *"what did our access policy say last Tuesday?"* is a query. It
receives four arguments (declare any subset): `method`, `path`, `key` (the
resolved caller name — key name, JWT `sub`, or mTLS CN; `''` when anonymous),
and `ip`.

```
PROCEDURE office_rules(method, path, key, ip)
  -- writes only from the ci key; reads for everyone
  WHEN method != 'GET' AND key != 'ci': RETURN 'deny:only ci may write here'
  RETURN 'allow'
END
```

The decision is the procedure's `RETURN` value:

- `'allow'` or `true` — proceed;
- `'deny'` / `'deny:<message>'` or `false` — 403, with the message;
- a record (e.g. a fact the policy read with `LET`) — honored as
  `{"allow": bool, "status": N, "message": "…", "add_headers": {…}}`:
  `status` defaults to 403 on deny; `add_headers` is applied to the
  **upstream** request on allow;
- anything else, a runtime error, or `FAIL 'msg'` — **403, fail closed**.

A route naming a missing or unparsable procedure is **rejected whole** at
config load (its paths 404) — a policy can never silently not run. Every
policy denial is audited with `"policy": "<name>"` in the fact, *even on
`audit: 'none'` routes* — a denial is a security event, not traffic volume.

Cost, measured: the procedure is parsed once per config load, so the hot
path only executes the parsed steps. `BenchmarkPolicyOverhead` puts the
two-step policy above at **≈2.1 µs per request** (Go stdlib interpreter,
2-core dev VM) — noise next to even a loopback HTTP round trip. Policies
that run CeQL reads (`LET x = FIRST FACTS OF …`) cost more, typically tens
of microseconds against the in-memory indexes.

Honesty: CePL is guards + CeQL reads/writes — no loops, no body access.
That covers allow/deny/annotate well; request/response *transformation* is
out of scope. Programmable, yes; a plugin marketplace, no.

## Webhook authorization

For allow/deny decisions that live outside the database — an entitlement
service, a corporate ACL — a route can call out to yours:

```
PUT route:api FACET config SET prefix='/api/', upstream='http://127.0.0.1:9000', auth='key', authz_webhook='https://authz.internal/check'
```

After built-in auth passes (including on `auth: 'none'` routes), the gateway
POSTs JSON to the webhook: `{"method", "path", "key", "ip", "headers"}`,
where `headers` is a fixed safe subset (`content-type`, `user-agent`,
`x-request-id`) — the caller's `Authorization` credential is **never**
forwarded to the webhook. A **2xx** answer allows the request; anything else
— a non-2xx status, the 2-second timeout, a refused connection — is a
**403, fail closed**, audited with `"webhook_denied": true`. Set
`webhook_fail_open: true` to prefer availability over enforcement when the
webhook itself is down (the default is deny, on purpose).

## Scaling out

One gateway is one writer on one control log. To run N gateways behind a load
balancer, share the control plane and keep audit local:

```
centauri gateway -data replica.log -addr :8080 -control-follow https://primary:7771 -control-token $PRIMARY_TOKEN
```

`-control-follow` keeps the gateway's control store as a **read-only replica**
of a primary Centauri server — the same log shipping `centauri follow` uses
(poll `/v1/log`, ingest raw bytes; the hash chain stays byte-identical to the
primary's). Routes and keys are managed on the *primary's* dashboard and reach
every follower within ~seconds; a follower refuses `-admin`, `-admin-inline`
and `-newkey` so nothing can write to the replica locally. Audit goes to a
separate local log per gateway (`-audit-data`, default: `-data` with a
`-audit.log` suffix), still tamper-evident and `centauri verify`-able.

Honest limits: the control plane stays single-writer (the primary), there is
no leader election or failover for it, and there is no built-in cross-gateway
audit merge — query each gateway's audit log, or ship them somewhere central
with `centauri follow`.

## Deploying on Kubernetes

Centauri is **not** a k8s ingress controller (no IngressClass, no Gateway API,
no CRDs). It deploys fine *on* Kubernetes as a normal workload: see
[`infra/k8s.yaml`](../infra/k8s.yaml) — a single-replica StatefulSet with a
persistent volume for the database (single-writer semantics), and a Deployment
+ Service for the gateway in single-port `-admin-inline` mode with
`CENTAURI_TOKEN` from a Secret and probes on `/livez` / `/readyz`.

## Flags reference

| flag | meaning |
|---|---|
| `-data` | control-plane log (routes, keys — and audit, unless `-control-follow`). |
| `-addr` | proxy listen address (default `:8080`). |
| `-admin` | serve the standard dashboard/API on this address over the same store. |
| `-admin-inline` | serve the dashboard on the proxy port for unmatched paths (single-port deploys). |
| `-token`, `-read-token` | admin API bearer tokens (write / read-only). |
| `-tls-cert`, `-tls-key` | HTTPS on the proxy listener. |
| `-mtls-ca` | require client certs signed by this CA on the proxy listener (needs the TLS pair). |
| `-control-follow` | follow this primary's log as a read-only control plane (scale-out). |
| `-control-token` | bearer token for the primary's `/v1/log`. |
| `-audit-data` | separate local audit log used with `-control-follow`. |
| `-newkey` | mint an API key offline and exit. |
| `-issuer` | `iss` claim of tokens minted at `POST /gw/token` (default `<scheme>://localhost<addr>`). |
| `-token-ttl` | lifetime, in seconds, of minted tokens (default 900). |
| `-token-aud` | `aud` claim of minted tokens (default `centauri`). |

## Audit & retention

With `audit: "meta"`, each request appends a fact — subject `gw:<route-name>`,
facet `gateway` — carrying `method`, `path`, `status`, `ms`, `key` (the key's
*name*, never the secret), `upstream`, `bytes_out`, and for LLM routes
`model` / token counts. Metadata only; request and response **bodies are never
recorded**.

Because each new fact supersedes the previous one on the same subject,
`FACTS OF gw:*` shows the *latest* request per route, and the full request log
is per-route history:

```
HISTORY OF gw:chat FACET gateway ORDER BY recorded DESC LIMIT 100
HISTORY OF gw:chat WHERE status >= 500          -- every 5xx, forever
```

**Audit facts grow the log** — one line per audited request, and nothing is
ever erased. Two levers:

```
# Mark old audit facts retired (dry-run by default; -apply to do it):
centauri retention -data gateway.log -pattern 'gw:*' -older-than 30 -apply
```

Honesty note: retention **RETIREs** — it appends `retired:true` corrections so
queries skip stale facts, but the log file does not shrink (append-only is the
point, and the hash chain stays intact). If audit volume is the problem, set
`audit: 'none'` on hot routes — those requests are simply not recorded.

## Demo it on Render (single port)

Render exposes one port per service, so use `-admin-inline`: proxied routes
win, and any path that matches no route serves the admin dashboard instead.

The repo's `render.yaml` already defines the service (`centauri-gateway`,
Docker, disk-backed, `CENTAURI_TOKEN` auto-generated). In the Render
dashboard: **Blueprints → your repo → Sync** (or New → Blueprint if starting
fresh), and the gateway deploys next to the database service.

Demo flow on the deployed URL:

1. Open `https://centauri-gateway-<id>.onrender.com/` — that's the dashboard
   (paste the service's `CENTAURI_TOKEN` once; the Gateway card is on the
   home screen). Or start at `/console` — the suite view, with every
   capability of the deployment on one screen.
2. Add a route live in the query bar — no restart:
   `PUT route:api FACET config SET prefix='/api/', upstream='https://your-backend', auth='none', rate_per_min=60`
3. Call `https://…onrender.com/api/...` and watch it proxy; then show the
   receipts: `HISTORY OF gw:api FACET gateway` and the time-travel closer:
   `FACTS OF route:api FACET config AS OF 5 minutes ago`.

Keyed routes on Render: mint the key in the **Console** (`/console` → Keys &
Access) — it works over the live admin API, so the writer lock is no obstacle.
Or store a key you choose yourself — compute its SHA-256 (PowerShell:
`Get-FileHash -InputStream ([IO.MemoryStream]::new([Text.Encoding]::UTF8.GetBytes("my-secret"))) -Algorithm SHA256`)
and `PUT gwkey:<hash> FACET config SET name='demo'` in the dashboard. Callers
then send `Authorization: Bearer my-secret`.

## Metrics

- The dashboard's **Gateway** card (admin plane, home screen) lists routes and
  a recent-traffic sample (requests, 5xx, avg ms) computed from audit facts.
- Your metrics are queries: `HISTORY OF gw:<route>` with `WHERE` / `ORDER BY` /
  `LIMIT` slices the request log any way you like. (CeQL aggregates —
  `COUNT`, `AVG` — run over *current* facts, not history, so cross-history
  roll-ups mean exporting `HISTORY` results and summing client-side. Honest
  limitation.)
- The admin plane serves Centauri's standard Prometheus endpoint at
  `/metrics` (process + API counters, no fact data).

## Centauri gateway vs Kong, honestly

| capability | Centauri gateway | Kong |
|---|---|---|
| Config time-travel (`AS OF` / `AS KNOWN AT` on live routing config) | ✓ native | ✗ (config history via external Git/CI discipline) |
| Tamper-evident request audit (hash-chained, `verify`-able) | ✓ | ✗ (logs are mutable files/streams) |
| One binary, zero dependencies, no separate datastore | ✓ | ✗ (NGINX/OpenResty; DB or declarative config tooling) |
| AI-native: LLM provider failover + model pinning + token accounting as facts | ✓ | partial (plugins exist; audit isn't a queryable temporal DB) |
| Raw proxy throughput | partial — cached per-route proxy on one tuned connection pool; measured ~26k req/s end-to-end on a 2-core dev VM (loopback upstream, the round trip dominates). Plenty for internal APIs; NGINX still wins at the edge | ✓ NGINX data plane, battle-tested at scale |
| Plugin ecosystem (auth, transforms, WAF, hundreds of plugins) | partial — built-in middleware set (headers, CORS, IP filters, body caps, path blocks) *plus* programmable CePL request policies and webhook auth callouts; no plugin marketplace | ✓ |
| Kubernetes ingress controller / service mesh | ✗ (deployable ON k8s — see infra/k8s.yaml — but not an ingress controller) | ✓ |
| OAuth2 / OIDC / mTLS client auth flows | partial — validates OIDC JWTs + mTLS client certs AND issues service tokens (client-credentials only: `/gw/token` + JWKS); no human login/consent flows — bring your IdP for people | ✓ |
| Enterprise scale-out, multi-DC control plane, commercial support | partial — N gateways can follow one primary control plane (`-control-follow`); no multi-DC, no leader election, no commercial support | ✓ |
| Response streaming (SSE) through LLM routes | ✓ passthrough, flushed per chunk; failover until the first byte | ✓ |

If you need raw throughput, a plugin ecosystem, or k8s ingress: use Kong (or
Envoy). If you need to answer *"what exactly was our routing config when the
incident started, who called what, and can we prove the audit wasn't edited?"*
— that's this.
