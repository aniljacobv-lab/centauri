# Centauri ⇄ Airbyte connectors

Plug Centauri into the [Airbyte](https://airbyte.com) ecosystem — without
adding a single dependency to the Centauri binary. These are standalone
programs that speak the [Airbyte Protocol](https://docs.airbyte.com/understanding-airbyte/airbyte-protocol)
over stdin/stdout and talk to a running Centauri over its HTTP API. Pure
Python standard library; no Airbyte CDK, no `pip install`.

| Connector | What it does |
|-----------|--------------|
| [`destination_centauri`](destination_centauri/) | **Land any of Airbyte's 300+ sources into Centauri as bi-temporal facts.** Each record becomes a `PUT` (`/v1/append`). Write one connector instead of N importers. |
| [`source_centauri`](source_centauri/) | **Stream Centauri facts out to any Airbyte destination.** Rides the CDC endpoint `/v1/changes`; Centauri's resumable byte-offset cursor maps directly onto Airbyte incremental `STATE` — only new facts, never duplicates. |

## Why this design

A connector is a separate process, exactly like Centauri's own MCP server:
it never links into the core binary, so the zero-dependency, single-binary
guarantee is untouched. The destination POSTs `{"events":[…]}` to
`/v1/append`; the source pages `GET /v1/changes?from=<cursor>` and emits the
cursor back as Airbyte `STATE`.

## Try it without Airbyte (raw protocol)

```bash
# 1. start Centauri
centauri serve            # http://localhost:7771

# 2. destination: pipe Airbyte RECORD/STATE messages in
printf '%s\n' \
  '{"type":"RECORD","record":{"stream":"users","data":{"id":7,"name":"Alice"}}}' \
  '{"type":"STATE","state":{"data":{"cursor":1}}}' \
| python3 destination_centauri/main.py write \
    --config dest-config.json --catalog catalog.json
# -> POSTs subject "users:7" as a fact, then echoes the STATE back

# 3. source: stream facts out (incremental, resumable)
python3 source_centauri/main.py read \
    --config src-config.json --catalog configured-catalog.json [--state state.json]
```

Minimal `dest-config.json` / `src-config.json`:

```json
{ "centauri_url": "http://localhost:7771", "token": "", "database": "" }
```

The destination also accepts `facet` (default `source`), `primary_key`
(record field used as the subject key; falls back to a per-stream counter),
`provenance` (default `SYSTEM_FEED`), and `batch_size` (default 500).

## Use it inside Airbyte

Build the images and register them as custom connectors:

```bash
docker build -t airbyte/destination-centauri:dev destination_centauri/
docker build -t airbyte/source-centauri:dev      source_centauri/
```

Then add each as a custom Docker connector in the Airbyte UI (Settings →
Sources/Destinations → New connector), or reference the image in your
`octavia`/API config. `spec`, `check`, `discover`, `read`, `write` all work.

## Status

v0.1.0 — append-only destination and incremental source. Tested end-to-end
against a mock API. Licensed with Centauri (PostgreSQL-style permissive);
built against Airbyte's open protocol, no Airbyte code is vendored.
