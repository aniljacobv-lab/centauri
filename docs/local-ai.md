# Centauri as a local-AI appliance

Centauri is a database **and** the local AI that runs over it — one binary, one
umbrella, no cloud. A business drops its data in; Centauri embeds, retrieves, and
answers using a local LLM it manages for you. Nothing leaves the machine, there
is no per-token bill, and the model is a managed dependency you can swap — not
something baked into the binary.

## How it works (and what is *not* in the binary)

- **Orchestration lives in Centauri** (stdlib Go, zero dependencies): retrieval,
  hybrid BM25 + vector ranking, RAG answering (`ASK`), enrichment (`ENRICH`),
  semantic search (`SEARCH`), and model selection.
- **Model weights do not.** A usable model is 2–20 GB and needs a GPU/SIMD
  runtime, so Centauri talks to a local **Ollama** server (OpenAI-compatible;
  LocalAI and vLLM work too) and pulls weights on first use. This keeps the
  single-binary, zero-dependency design intact and lets you change models without
  rebuilding Centauri.

A model is registered as a plain fact (`model:<name>` with a `config` facet), so
the choice is data — versioned, queryable, auditable — not a config file.

## One command: the tiered appliance

```
centauri serve -ai auto       # detect hardware, register the right local models
centauri serve -ai small      # force a tier
centauri serve -ai balanced
centauri serve -ai max
```

`-ai` registers a **chat**, an **embedding**, and a **vision** model sized to the
tier, then prints the `ollama pull` commands to fetch them once. Registration is
idempotent — restarting never churns the log, and deleting the `model:*` facts
lets you re-tier. `auto` reads total system memory (Linux `/proc/meminfo`; other
platforms default to `small`, override explicitly).

## Hardware tiers

| Tier | Target hardware | Chat | Embedder | Vision |
|---|---|---|---|---|
| `small` | ~8 GB RAM, CPU-ok (laptop / small server) | `gemma3:4b` | `nomic-embed-text` | `gemma3:4b` |
| `balanced` | 12–16 GB GPU workstation | `qwen3:14b` | `bge-m3` | `gemma3:12b` |
| `max` | 24 GB+ GPU (RTX 4090 / well-specced Mac) | `qwen3:32b` | `bge-m3` | `gemma3:27b` |

`small` makes "any small business" credible — it runs on a laptop with no GPU.
`max` is near-frontier quality, still fully local.

## The model evaluation: which model for which Centauri job

Match the model to the *task*, not one model for everything. Recommendations
reflect the June 2026 local-model landscape.

| Centauri job | Recommended model(s) | Why |
|---|---|---|
| Embeddings / retrieval (the RAG core) | **nomic-embed-text** (137M, ~274 MB, runs on CPU) as the default; **BGE-M3** for multilingual / hybrid dense+sparse retrieval | In RAG, retrieval quality matters more than LLM size — a good embedder plus clean chunking beats a giant model with poor retrieval. nomic is the most-pulled local embedder; BGE-M3 leads multilingual. |
| `ASK` / RAG answers, summarization | **Qwen3** (Apache-2.0, strong all-round, 100+ languages) | Best overall local family for quality, sizing options, and a permissive commercial license. |
| `ENRICH` vision / document extraction (invoices, PDFs) | **Gemma 3** (4B+ are multimodal, 140+ languages, 128K context) | Strong multimodal quality even at small sizes — Gemma 3 4B "punches above its weight." |
| NL→CeQL translation, classify/tag on ingest | **Gemma 3 4B** or **Phi-4-mini (3.8B)** | Cheap and fast; Centauri's deterministic rules run first anyway, so the model only handles the long tail. |
| Heavy reasoning / query optimization | the largest you can run (**Qwen3 32B**) | Reasoning scales with size; use only when the cheaper model isn't enough. |

Approximate VRAM (Q4 quantization): 3–4B ≈ 3 GB, 8B ≈ 6 GB, 12–14B ≈ 10 GB,
32B ≈ 20–22 GB.

A note on naming: there is no real "GLM-5.2" today; the GLM-4.x family is a
credible alternative, but **Qwen3** and **Gemma 3** are the practical local
defaults, which is why the presets use them. Model lineups change monthly — the
tier table is config, not a contract; edit `internal/ai/presets.go` to track new
releases.

## What "learning" means here

The appliance learns by **accumulating facts**, not by retraining weights:
documents, embeddings, the model's answers, and (next increment) user feedback
all live in the append-only log, so retrieval and prompting improve over time and
every answer can be traced to its sources. For true fine-tuning, Centauri's role
is to *produce the dataset* (curated, versioned, exportable) and hand it to an
external trainer — the data stays in Centauri, training happens out of process.

## Roadmap

1. **Tiered model bootstrap** (`serve -ai`) — *done*.
2. **Auto-enrich on ingest** — *done*. With `serve -ai` on, every new fact embeds
   itself in the background (via the registered embedder) right after it commits,
   so data is instantly `SEARCH`/`SIMILAR`/`ASK`-able with no manual `ENRICH`.
   Embeddings are ordinary enrichment facts, so this never touches the original
   write's hash chain — it just appends more facts. Covers both ingest paths
   (`/v1/append` and CeQL `PUT`); a no-op if no embedder is registered.
3. **Feedback loop** — *done*. A thumbs-up/down on a source (`POST /v1/feedback`
   `{event, score, note}`, score in [-1, 1]) is stored as an append-only feedback
   fact; `retrieve()` adds a bounded nudge so liked sources rise and distrusted
   ones sink in future RAG answers and SEARCHes. No model weights change — the
   appliance improves on your data purely by accumulating facts, and every rating
   is a timestamped, replayable fact. The flow: `ASK` returns source ids → user
   rates one → next `ASK` re-ranks. (Remaining polish: a 👍/👎 control in the
   dashboard, and an optional CeQL `RATE` keyword; the REST endpoint works today.)

---

Sources (June 2026 local-model landscape):
[HF: open-weight models to run locally](https://huggingface.co/blog/daya-shankar/open-source-llm-models-to-run-locally),
[Ollama VRAM requirements 2026](https://localaimaster.com/blog/ollama-model-ram-vram-table),
[Best Ollama models, June 2026](https://www.morphllm.com/best-ollama-models),
[Best embedding models for RAG 2026 (Milvus)](https://milvus.io/blog/choose-embedding-model-rag-2026.md),
[Ollama embedding models benchmarked](https://www.morphllm.com/ollama-embedding-models).
