# Vision setup — one package

Centauri's vision feature (let an AI read images / PDFs / electrical drawings)
needs two local helpers it can't bundle into the binary (they're large and
updated independently), but it installs and runs them *for* you:

- **Ollama** — runs the multimodal model locally (`llava`) and an embedder
  (`nomic-embed-text`). This is what "sees" the image.
- **A PDF rasteriser** — poppler (`pdftoppm`) or ImageMagick (`magick`) — to turn
  PDF pages into images. *Only needed for PDFs; plain images work without it.*

## The one-click way (recommended)

Just run **`run-centauri.bat`**. On launch it:

1. **Offers to install** the missing pieces (Ollama + a PDF renderer) and pulls
   the models — with your permission, in a separate window, skipping anything
   already installed. (Under the hood it calls `centauri setup vision -install`.)
2. **Auto-starts Ollama** if it isn't already running, and **stops it when you
   close Centauri** — but only the Ollama *it* started; an Ollama you were
   already running is left alone. (Disable with `centauri desktop -ollama=false`.)

So as a user you don't run separate commands or manage processes — it's one
package: install once, and Ollama's lifecycle follows Centauri's.

## The manual way

Run this on the machine where Centauri serves (vision is a **local** feature —
no cloud):

```
centauri setup vision -install
```

That uses your OS package manager to install whatever's missing, then pulls the
Ollama models. Drop `-install` to just check status and get exact commands (it
exits non-zero if something's missing, which is how `run-centauri.bat` knows to
offer setup).

| OS | What it installs |
|----|------------------|
| Windows | `winget install Ollama.Ollama` + `ImageMagick.ImageMagick` + `ArtifexSoftware.GhostScript` (ImageMagick needs Ghostscript to read PDFs; **poppler** also works and needs neither) |
| macOS | `brew install ollama` + `brew install poppler` |
| Linux | `apt-get install poppler-utils`; Ollama via `curl -fsSL https://ollama.com/install.sh \| sh` |

**Windows PDF gotcha:** don't rely on the bare `convert` command — on Windows that's the built-in System32 *filesystem* tool, not ImageMagick (it'll fail with "exit status 4"). Centauri ignores it and uses `pdftoppm` (poppler) or `magick` (ImageMagick v7 + Ghostscript). Poppler is the simplest: it rasterises PDFs with no extra dependency.

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
- `centauri desktop` manages Ollama's lifecycle (start if needed, stop on exit);
  pass `-ollama=false` to opt out and manage Ollama yourself. `centauri serve`
  (headless/cloud) never auto-manages Ollama.
- Everything stays on your machine — no Firestore, no external object store.
