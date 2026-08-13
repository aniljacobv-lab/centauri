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
| `auth`         | string | `"none"` (open) or `"key"` (requires a minted API key).                                          |
| `rate_per_min` | number | Requests allowed per minute on this route; absent/0 = unlimited.                                 |
| `timeout_ms`   | number | Upstream timeout in milliseconds.                                                                |
| `audit`        | string | `"meta"` writes one audit fact per request (metadata only, never bodies); `"none"` writes nothing. |
| `llm`          | bool   | Treat as an OpenAI-compatible LLM route with provider failover (see below).                      |
| `providers`    | array  | LLM only: `{endpoint, model, auth_env, auth_file}` entries, tried in order.                      |
| `retired`      | bool   | Set by `RETIRE` — the route stops matching; its history remains queryable.                       |

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

**MVP limitation, stated plainly:** `stream: true` is forced to `false` — the
gateway buffers the full response before returning it. No token-by-token
streaming yet. If your client needs SSE streaming, go direct to the provider
for now.

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
| Raw proxy throughput | ✗ Go stdlib proxy, unoptimized | ✓ NGINX data plane, battle-tested at scale |
| Plugin ecosystem (auth, transforms, WAF, hundreds of plugins) | ✗ | ✓ |
| Kubernetes ingress controller / service mesh | ✗ | ✓ |
| OAuth2 / OIDC / mTLS client auth flows | ✗ (API keys only) | ✓ |
| Enterprise scale-out, multi-DC control plane, commercial support | ✗ | ✓ |
| Response streaming (SSE) through LLM routes | ✗ in MVP (forced off) | ✓ |

If you need raw throughput, a plugin ecosystem, or k8s ingress: use Kong (or
Envoy). If you need to answer *"what exactly was our routing config when the
incident started, who called what, and can we prove the audit wasn't edited?"*
— that's this.
