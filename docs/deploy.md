# Deploying Centauri as a web app

The dashboard ships inside the binary, so "making it a website" is just
running the binary somewhere public and putting HTTPS in front. Three
recipes, lightest first.

## Security checklist (read before exposing anything)

- **Always set `-token`** (writes/admin) and ideally `-read-token`
  (viewers/dashboards). Without a token, anyone with the URL owns your data.
- **TLS:** Centauri can terminate HTTPS itself — pass `-tls-cert <pem> -tls-key <pem>`
  to `serve`/`desktop`/`serve -lazy-index` (no reverse proxy required). A reverse
  proxy (Caddy/nginx) is still a fine choice for defense in depth, automatic certs,
  and HTTP/2; if you use one, bind Centauri to localhost. There is no mTLS / client-cert
  auth or hot cert reload yet.
- Take periodic `centauri backup` snapshots off the box, and record the
  chain head somewhere separate (tamper evidence needs an external anchor).
- **Admission control:** `-max-concurrency` (HTTP 429 over the cap) and
  `-query-timeout` seconds (HTTP 503) apply to the normal `serve`/`desktop` hot
  path and `serve -lazy-index`; `-max-concurrency-per-db` adds a per-tenant
  (per `?db=`) cap. Streaming endpoints (`/v1/watch`, `/v1/changes`, `/v1/log`)
  and health/metrics are exempt.
- **Probes & metrics:** both `serve` and `serve -lazy-index` expose `/livez`,
  `/readyz`, and a Prometheus `/metrics` endpoint (unauthenticated — no fact data),
  so Kubernetes probes and Prometheus scraping work in either mode.
- **Structured logs:** `-log-format json -log-level info` emits one slog line per
  request with an `X-Request-ID` correlation id (honoured inbound, echoed back).

## Modes &amp; flags cheat-sheet

- **Write scaling.** `-group-commit` coalesces concurrent appends into one fsync
  (single node, higher throughput under load). `serve -shards N` (with `-data` a
  directory) partitions subjects across N independent shard logs and writes them
  in parallel (~N× throughput): `POST /v1/append`, routed `current`/`history`/
  `asof`, `/v1/subjects`, `/v1/shards`, and `/v1/query` for a *concrete* subject.
  Wildcard/global CeQL, cross-shard SEARCH, and cross-shard atomic writes are not
  supported in sharded mode — use single-store `serve` for those.
- **SQL front door.** `POST/GET /v1/sql` accepts a read-only `SELECT` subset
  (WHERE/GROUP BY/HAVING/ORDER BY/LIMIT, plus `AS OF` and `FOR SYSTEM_TIME AS OF`)
  and transpiles to CeQL. Not a wire protocol — BI tools still need an adapter.
- **Retention &amp; legal hold.** `centauri retention -pattern '<glob>' -older-than N`
  previews; add `-apply` to RETIRE stale subjects (history kept, never erased).
  A `hold:<name>` fact carrying a subject `pattern` puts matching subjects under a
  legal hold that retention skips. Schedule the `-apply` form for a recurring policy.

## Read-only cold tier — `serve -lazy-index`

For datasets larger than RAM, archive the log (`centauri archive -data <log> -to <dir>`)
and serve the archive read-only: `centauri serve -lazy-index -data <dir>`. Only current
facts + zone maps stay resident; the data routes honour `-read-token`, and the
Tablespace Console dashboard, health probes, and metrics are served too. See
[demo-tablespaces.md](demo-tablespaces.md).

## Recipe 1 — VPS + Caddy (~20 minutes, ~$5/month)

Any small Linux VM (Hetzner, DigitalOcean, Lightsail). Point a DNS A
record (e.g. `db.example.com`) at it first.

```bash
# on the server
curl -LO https://github.com/aniljacobv-lab/centauri/releases/latest/download/centauri-v0.3.0-linux-amd64
chmod +x centauri-v0.3.0-linux-amd64 && sudo mv centauri-v0.3.0-linux-amd64 /usr/local/bin/centauri

# run as a service, bound to localhost only
sudo tee /etc/systemd/system/centauri.service <<'EOF'
[Unit]
Description=Centauri database
After=network.target
[Service]
ExecStart=/usr/local/bin/centauri serve -data /var/lib/centauri/centauri.log -addr 127.0.0.1:7771 -token CHANGE_ME -read-token CHANGE_ME_TOO
Restart=on-failure
StateDirectory=centauri
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now centauri

# Caddy: automatic HTTPS in three lines
sudo apt install -y caddy
sudo tee /etc/caddy/Caddyfile <<'EOF'
db.example.com {
    reverse_proxy 127.0.0.1:7771
}
EOF
sudo systemctl reload caddy
```

Open `https://db.example.com`, paste your read or admin token into the
dashboard's token box once — done. Your laptop can keep a live local
replica: `centauri follow -primary https://db.example.com -token ...`.

## Recipe 2 — Fly.io / Railway / Render (no server admin)

The repo's `Dockerfile` works on any container platform. Fly.io example:

```bash
fly launch --no-deploy        # accept defaults; it detects the Dockerfile
fly volumes create centauri_data --size 1
# fly.toml: mount the volume at /data; set [env] or secrets:
fly secrets set CENTAURI_TOKEN=CHANGE_ME
fly deploy
```

These platforms give you HTTPS automatically. Same token rules apply.

## Recipe 3 — docker-compose (your own box, one file)

```yaml
services:
  centauri:
    build: .
    command: ["serve", "-data", "/data/centauri.log", "-addr", ":7771", "-token", "${CENTAURI_TOKEN}"]
    volumes: ["centauri-data:/data"]
    ports: ["127.0.0.1:7771:7771"]   # localhost only; proxy provides TLS
    restart: unless-stopped
volumes:
  centauri-data:
```

## What you get once it's up

The complete product at one URL: dashboard, Genesis, CeQL workbench,
textbook (`/ceql`), REST API, SSE watch streams — plus replication
(`follow`), chain-verified `backup`, and per-token access tiers. Multiple
people share it; agents connect to it; your laptop replicates it.
