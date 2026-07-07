# Local AI features — private chat, search, vision, and plain English

Centauri's engine embeds no model and stays zero-dependency; the AI features
orchestrate models you run yourself through [Ollama](https://ollama.com) over
plain HTTP. The easy path is **zero-step**: run `centauri desktop` (or click
the dashboard's **AI panel**, or `POST /v1/ai/enable`) and Centauri installs
the runtime if needed, picks a tier for your hardware, and downloads a model
trio in the background. Everything runs on your machine; nothing leaves it,
and there is no per-token bill.

Every feature **degrades gracefully** — if no suitable model is registered (or
it isn't running), the non-LLM path answers instead, so queries never break.

## The tiers

| Tier | Hardware | Chat | Embeddings | Vision |
|------|----------|------|------------|--------|
| `small` | ~8 GB RAM, CPU-only OK | `gemma3:4b` | `nomic-embed-text` | `gemma3:4b` |
| `balanced` | ~12–16 GB GPU | `qwen3:14b` | `bge-m3` | `gemma3:12b` |
| `max` | 24 GB+ GPU (RTX 4090 / big Mac) | `glm-4.7-flash` | `bge-m3` | `gemma3:27b` |

`auto` (the default) detects your memory and picks. `glm-4.7-flash` is a 30B
MoE model (MIT-licensed, 200K context) — the strongest chat model that fits on
a workstation.

## What you get

### 1. Semantic search — and auto-embedding
`SEARCH '<text>' OF <pattern>` goes beyond keywords two ways:

- it searches **AI-derived text** (vision descriptions, tags, summaries), so an
  analysed image or PDF is findable by what's *in* it;
- it **embeds your query** with the local embedder and blends vector similarity
  with BM25 — matches by meaning, even with no shared keywords.

With local AI enabled, **new facts embed themselves in the background** as you
write them — no ENRICH run needed before `SIMILAR` and hybrid `SEARCH` work.

```
SEARCH 'inventory stockout analysis' OF asset:*
SEARCH 'household products in a basket' OF asset:*
```

Without an embedder registered, SEARCH is pure BM25 keyword ranking.

### 2. ASK your own data (RAG)
`ASK '<question>'` answers from the built-in knowledge base first; if it
doesn't know, it **retrieves your most relevant facts** (hybrid search) and has
the local LLM answer **using only those, with citations**.

```
ASK 'how much does the widget cost'
ASK 'summarize the history of item:1'
ASK 'why do these prices disagree'
```

The result carries `grounded: true`, the `answer`, and `sources` (the event ids
the model used). Without a chat model, ASK logs the question as a knowledge-gap
fact instead of guessing.

### 3. ENRICH — AI inside the query
`ENRICH <pattern> USING <model> [ON <field>] [AS <kind>]` runs a registered
model over matching events and stores the result as a superseding, provenanced
**enrichment fact** — inference cached in the log, re-runs skip what's done.

```
ENRICH item:*  USING embed                     -- embeddings → vector index
ENRICH ticket:* USING summarize ON body AS summary
ENRICH asset:*  USING vision                   -- describe images/PDF pages
```

### 4. Vision — files an AI can read
Upload an image or PDF (`POST /v1/assets` or the dashboard); pages are rendered
locally, a vision model describes them, and the description is embedded into
the vector index — so `SEARCH 'E-3 200A panel'` finds the drawing. Setup:
[vision-setup.md](vision-setup.md).

### 5. Plain English → CeQL
The **✨ helper** (and `POST /v1/assist`) translates natural language to CeQL:
fast deterministic rules first, then the local LLM for the rest. The model's
output is re-parsed before it's offered, so a suggestion is never invalid CeQL.

## The dashboard AI panel and /v1/ai endpoints

| Call | What it does |
|------|--------------|
| `GET /v1/ai/status` | provisioning progress, whether the runtime is up, and **where chat answers are computed** (local / cloud / none) |
| `POST /v1/ai/enable {"tier":"auto\|small\|balanced\|max"}` | turn on fully-local AI; models download once, in the background |
| `POST /v1/ai/cloud {"provider":"zai\|openai\|anthropic\|custom", "api_key":"...", "model":"...", "endpoint":"..."}` | switch chat to another AI: z.ai (GLM-5.2), OpenAI (GPT-5.5), Anthropic (Claude) — each with its own key, stored in its own file — or any custom OpenAI-compatible server (key optional; a LAN Ollama box works keyless). Default provider: `zai`. See warning below |
| `POST /v1/ai/local {"tier":"auto"}` | switch back to local — the reversible off-switch |

All four are admin-token-only.

## Registering models by hand

Models are ordinary config facts — register anything OpenAI-compatible or
Ollama-served, per database (the db dropdown):

```
PUT model:chat FACET config SET endpoint='http://localhost:11434/v1/chat/completions',
  kind='chat', model='qwen3:14b'

-- a remote endpoint with a secret: name a source, never the key itself
PUT model:summarize FACET config SET endpoint='https://api.openai.com/v1/chat/completions',
  kind='chat', model='gpt-4o-mini', auth_env='OPENAI_KEY'      -- read from the environment
PUT model:cloud FACET config SET endpoint='https://api.z.ai/api/paas/v4/chat/completions',
  kind='chat', model='glm-5.2', auth_file='/path/to/zai.key'   -- read from a 0600 file
```

`auth_env` names an environment variable; `auth_file` names a file (the cloud
opt-in writes one, mode 0600, next to the data file). **The key itself never
enters the log** and is never echoed back. Slow first call? The model
cold-loads; raise the ceiling per model with `timeout_secs='600'`.

Model kinds: `chat`, `embedding`, `vision`.

## The GLM-5.2 cloud boost — read before pasting a key

The local AI (Ollama on this machine) never needs an API key. Hosted
providers each keep their own key — pasting a new provider's key never
disturbs another's. Pasting a provider API key (AI panel or `POST /v1/ai/cloud`)
switches chat to **GLM-5.2**, a much stronger model. Be clear about the trade:

- **Your prompts (and retrieved facts) leave your machine** and are processed
  by Z.ai under their terms. Local mode sends nothing anywhere.
- **It costs money** — per-token API pricing, unlike the free local tiers.
- **GLM-5.2 cannot run locally.** The weights are 200+ GB; no workstation tier
  will ever serve it. Local tops out at `glm-4.7-flash`.
- It's **off by default, clearly marked, and reversible** (`POST /v1/ai/local`).
  Embeddings and vision stay local either way.
