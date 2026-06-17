# source-centauri

An Airbyte **source** that streams Centauri facts via the CDC endpoint.

One stream, `facts`. `read` pages `GET /v1/changes?from=<cursor>`, emits one
`RECORD` per event, and emits the returned byte-offset `cursor` as Airbyte
`STATE`. Save that state and the next sync resumes from exactly where it left
off — no missed or duplicated facts. Supports `full_refresh` (from 0) and
`incremental` (from saved cursor).

## Config

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `centauri_url` | yes | `http://localhost:7771` | Centauri HTTP API base URL |
| `token` | no | – | Bearer token if the server requires one |
| `database` | no | – | Named environment (`?db=`) |

## Commands

```bash
python3 main.py spec
python3 main.py check    --config config.json
python3 main.py discover --config config.json
python3 main.py read     --config config.json --catalog configured-catalog.json [--state state.json]
```

Each emitted record's `data` is the full event: `event_id`, `subject`,
`facet`, `type`, `value`, `effective_time`, `recorded_time`, `provenance`,
`confidence`.
