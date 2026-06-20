# Local-LLM features — search, ASK, and plain English

Once you register a local model (see [vision-setup.md](vision-setup.md) — one
click in **📎 Vision → Register model:vision**, which also writes an embedder),
Centauri uses it for far more than describing images. Everything runs on your
machine through [Ollama](https://ollama.com); nothing leaves it, and there's no
API cost. Each feature **degrades gracefully** — if no suitable model is
registered (or it's not running) it falls back to the non-LLM path, so queries
never break.

## What you get

### 1. Semantic search
`SEARCH '<text>' OF <pattern>` now does two things beyond keyword matching:

- It searches the **AI-derived text** (vision descriptions, tags, summaries),
  not just raw fact values — so an analysed image/PDF is findable by what's *in*
  it.
- It **embeds your query** with the local embedder and ranks every candidate by
  vector similarity, blended with BM25 — so you find things by **meaning**, even
  with no shared keywords.

```
SEARCH 'inventory stockout analysis' OF asset:*
SEARCH 'household products in a basket' OF asset:*
```

Without an embedder registered, SEARCH is pure keyword (the previous behavior).

### 2. ASK your own data (RAG)
`ASK '<question>'` answers from the built-in knowledge base first; if it doesn't
know, it **retrieves your most relevant facts** (hybrid search) and has the
local LLM answer **using only those, with citations** to the source events.

```
ASK 'how much does the widget cost'
ASK 'summarize the history of item:1'
ASK 'why do these prices disagree'
```

The result carries `grounded: true`, the `answer`, and `sources` (the event ids
the model used). Without a chat model registered, ASK logs the question as a
knowledge gap (the previous behavior).

### 3. Plain English → CeQL
The **✨ Ask Copilot** box (and `POST /v1/assist`) translates natural language to
CeQL: fast deterministic rules first, then the **local LLM** for anything the
rules don't cover. The model's output is re-parsed before it's offered, so a
suggestion is never invalid CeQL.

### 4. Reasoning, in plain English
Folded into ASK (#2): ask in natural language and RAG pulls the right facts for
the model to summarize, explain, or narrate — no special commands.

## Which models are used

| Feature | Model kind it looks for |
|---------|-------------------------|
| Semantic search, RAG retrieval | a registered **`embedding`** model (e.g. `nomic-embed-text`) |
| ASK answers, NL→CeQL | a registered **`chat`** or **`vision`** model (e.g. `llava`) |

The one-click **Register model:vision** button writes both (`model:vision` +
an embedder). To use a different/faster text model for ASK, register one with
`kind='chat'`, e.g.:

```
PUT model:chat FACET config SET endpoint='http://localhost:11434/v1/chat/completions',
  kind='chat', model='llama3.1'
```

Models, embeddings, and answers are all **per-database** — register and query in
the same environment (the db dropdown). Slow first call? The model cold-loads;
raise the ceiling per model with `timeout_secs='600'`.
