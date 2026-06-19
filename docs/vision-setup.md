# Vision setup — one command

Centauri's vision feature (let an AI read images / PDFs / electrical drawings)
needs two local helpers it can't bundle (they're big and updated independently):

- **Ollama** — runs the multimodal model locally (`llava`) and an embedder
  (`nomic-embed-text`). This is what "sees" the image.
- **A PDF rasteriser** — poppler (`pdftoppm`) or ImageMagick (`magick`) — to turn
  PDF pages into images. *Only needed for PDFs; plain images work without it.*

Run this on the machine where Centauri serves (vision is a **local** feature —
it does not use any cloud):

```
centauri setup vision -install
```

That uses your OS package manager to install whatever's missing, then pulls the
Ollama models. Drop `-install` to just check status and get exact commands.

| OS | What it installs |
|----|------------------|
| Windows | `winget install Ollama.Ollama` + `winget install ImageMagick.ImageMagick` |
| macOS | `brew install ollama` + `brew install poppler` |
| Linux | `apt-get install poppler-utils`; Ollama via `curl -fsSL https://ollama.com/install.sh \| sh` |

Then:

1. `centauri desktop` (or `centauri serve`)
2. open **📎 Vision** → click **Register model:vision** (one click; writes the
   `model:vision` + embedder config as facts)
3. **Upload** an image or PDF → **Run ENRICH** → search it:
   `SEARCH 'E-3 200A panel'` · `FACTS OF asset:*` · `SIMILAR TO <asset>`

Notes:
- The model server must be reachable from the Centauri process. The default
  endpoint `http://localhost:11434` means Ollama on the **same host**.
- The first `ollama pull llava` is a multi-GB download (one-time).
- Everything stays on your machine — no Firestore, no external object store.
