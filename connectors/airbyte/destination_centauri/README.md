# destination-centauri

An Airbyte **destination** that lands records as Centauri facts.

Each Airbyte `RECORD` becomes one fact appended via `POST /v1/append`:

- **subject** = `<stream>:<key>` — `key` is the record's `primary_key` field
  if configured, else a per-stream counter.
- **facet** = the `facet` config (default `source`).
- **type** = `OBSERVED`; **value** = the full record; **confidence** = 1.0;
  **provenance** = the `provenance` config (default `SYSTEM_FEED`).

`STATE` messages are acknowledged back to Airbyte *only after* the records
before them are durably appended — the contract that makes resumable syncs
safe. Records are sent in batches of `batch_size` (default 500).

## Config

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `centauri_url` | yes | `http://localhost:7771` | Centauri HTTP API base URL |
| `token` | no | – | Bearer token if the server requires one |
| `database` | no | – | Named environment (`?db=`) |
| `facet` | no | `source` | Facet to write on |
| `primary_key` | no | – | Record field used as the subject key |
| `provenance` | no | `SYSTEM_FEED` | `SYSTEM_FEED`/`HUMAN_ENTRY`/`SCAN_VERIFIED`/`AI_INFERRED` |
| `batch_size` | no | `500` | Facts per `/v1/append` request |

## Commands

```bash
python3 main.py spec
python3 main.py check  --config config.json
python3 main.py write  --config config.json --catalog catalog.json < messages.jsonl
```
