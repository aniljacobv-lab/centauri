# Design: vision — local assets an AI can see

**Status:** MVP building · **Builds on:** ENRICH (`internal/ceql/enrich.go`), the
vector index (`internal/store/search.go`), enrichment facts, and on-disk
payloads.

## The idea

Drop an image or document (a photo, a scanned form, a large **electrical
drawing** PDF) into Centauri. A vision model inspects it; Centauri stores the
**blob locally**, the model's **structured analysis + description**, and a
**vector embedding** — all as ordinary facts in the same database. An agent then
finds and reasons over the drawing with one `SEARCH`/`SIMILAR` query, **with no
Firestore round-trip** and no separate object store: the bytes, the meaning, and
the vector live together, locally.

This is not a new subsystem — it's **multimodal ENRICH + a local asset store**.
Centauri still embeds no model (invariant 5); it orchestrates an external vision
endpoint over HTTP, exactly as text ENRICH already does.

## Storage layout

```
<datadir>/
  centauri.log              # the facts (unchanged)
  assets/
    <sha256>.png            # the original blob, content-addressed
    <sha256>.pdf
    <sha256>-p1.png         # rendered PDF pages
    <sha256>-p2.png
```

Blobs never go in the JSONL log (binaries would bloat it and break the
text/hash-chain model). The log holds only a small **reference fact**:

- **Image** → `asset:<sha>` facet `vision`
  `{kind:"image", mime, filename, bytes, sha256, image_path}`
- **PDF** → `asset:<sha>` facet `doc` `{kind:"pdf", mime, filename, pages, sha256}`
  plus, per rendered page, `asset:<sha>-p<N>` facet `vision`
  `{kind:"page", page:N, parent:"asset:<sha>", mime:"image/png", image_path}`

`image_path` is the absolute path of the blob the vision model should read.
Content-addressing (sha256) means re-uploading the same file is a no-op.

## Flow

```
POST /v1/assets            upload bytes → store blob, render PDF pages, write asset fact(s)
GET  /v1/assets/<sha>      serve a blob (UI preview / agent fetch)
ENRICH asset:* USING <m>   vision model reads each image-bearing fact → analysis fact (+ embedding)
SEARCH 'E-3 200A panel'    BM25 over descriptions + vector SIMILAR — finds the sheet
```

### Vision ENRICH

A vision model is registered as a fact, like any model:

```
PUT model:drawing FACET config SET
  endpoint='http://localhost:11434/v1/chat/completions',
  kind='vision', model='llava', auth_env='OLLAMA_KEY',
  prompt='You are reading an electrical drawing. Return JSON:
          {"description": "...", "tags": ["..."], "fields": {"sheet":"", "panels":[], "ratings":[]}}',
  embed_with='nomic'      -- optional: embed the description for SIMILAR
```

`ENRICH asset:* USING drawing` then, for every current fact carrying an
`image_path`:

1. reads the blob from disk, base64-encodes it,
2. sends it in the OpenAI vision message shape
   (`content:[{type:text…},{type:image_url, image_url:{url:"data:<mime>;base64,…"}}]`),
3. parses the reply as JSON (fences tolerated) into `{description, tags, fields}`
   — falling back to `{description: <raw text>}` if it isn't JSON,
4. stores it as an **enrichment** (`kind` defaults to `vision`), and
5. if `embed_with` is set, embeds the description via that embedder model and
   stores an `embedding` enrichment → it flows into the **vector index**.

Because enrichments are cached facts, re-running `ENRICH` skips assets already
analysed — inference is paid once, then it's just data.

### Querying

- `SEARCH '<text>'` — full-text over descriptions/tags + vector SIMILAR (hybrid,
  already built).
- `SIMILAR TO asset:<sha>` — visually/semantically similar assets.
- `FACTS OF asset:* WHERE any MATCHES 'transformer'` — structured/text filter.
- The MCP server exposes the same queries, so an agent searches the drawings
  locally.

## Zero-dependency boundary (PDF rendering)

Rendering a PDF page to an image needs a rasteriser. Centauri keeps **zero Go
dependencies** (`go.mod` require block stays empty) by **shelling out** (stdlib
`os/exec`) to an external tool detected at runtime — in order of preference:
`pdftoppm`/`pdftocairo` (poppler), then `magick`/`convert` (ImageMagick). If none
is present, the PDF is still stored and a `doc` fact written, but page rendering
is skipped and the upload response says how to enable it (install poppler).
Raster images (PNG/JPG/WEBP/GIF) need no external tool.

This is a deliberate trade: "PDF rendering built in" to the *workflow* (Centauri
drives it) without a *Go module* dependency.

## What the MVP includes / defers

**Includes:** asset store + `/v1/assets` upload & fetch; PDF→page rendering via
external tool; the `vision` model kind in ENRICH (image sourcing, structured
parse, `embed_with`); searchability via the existing vector/BM25 paths; a Studio
upload-and-analyse control; tests with a stubbed `Infer`.

**Defers:** native image (CLIP) embeddings (we embed the text description for
now); auto-tiling huge drawings into regions for fine detail; OCR as a separate
signal; schema-validated structured fields per document type (the `fields`
object is free-form in v1).

## Invariants preserved

Nothing is erased (re-analysis supersedes the prior enrichment); the hash chain
covers only the reference facts (blobs live beside the log, like `--lazy`
payloads); replay determinism is unaffected (the asset store is content-addressed
and rebuilt from facts); zero Go deps (the model and the PDF rasteriser are both
external, reached over HTTP / `os/exec`).
```
