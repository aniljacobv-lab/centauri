# Deploying Centauri as a web app

The dashboard ships inside the binary, so "making it a website" is just
running the binary somewhere public and putting HTTPS in front. Three
recipes, lightest first.

## Security checklist (read before exposing anything)

- **Always set `-token`** (writes/admin) and ideally `-read-token`
  (viewers/dashboards). Without a token, anyone with the URL owns your data.
- **Never expose port 7771 directly.** Centauri speaks plain HTTP; run a
  reverse proxy (Caddy/nginx) for TLS. Bind Centauri to localhost only.
- Take periodic `centauri backup` snapshots off the box, and record the
  chain head somewhere separate (tamper evidence needs an external anchor).
- This is a v0.x single-node server: no rate limiting, no WAF. Treat it
  like an internal tool with a password, not a hardened public API.

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
