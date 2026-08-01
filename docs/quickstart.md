# Centauri quickstart — your own private AI in about 5 minutes

This guide assumes nothing. You don't need to know what a model, an API key,
or a database is. If you can install a program and open a browser, you can
run Centauri.

## What Centauri is

Centauri is **your own AI with a perfect memory, running on your computer**.

- You put in the things your business runs on — notes, customer details,
  meeting summaries, documents.
- You ask questions in plain English, like you'd ask a smart employee.
- It answers from *your* information and remembers everything, forever —
  nothing you put in is ever lost or silently changed.

Everything happens on your machine. Your data does not go to the internet,
there is no account to create, and there is no monthly AI bill.

## Install it (Windows)

1. Download **`centauri-windows-setup.exe`** from the
   [Releases page](https://github.com/aniljacobv-lab/centauri/releases).
2. Double-click it and click through the installer (the defaults are fine).
3. That's it. You now have **Centauri** in your Start Menu (and on your
   desktop, if you left that box ticked).

Prefer no installer? Download the **`centauri-windows-amd64.zip`** instead,
unzip it anywhere, and double-click **`run-centauri.bat`**.

On a Mac or Linux machine: download the binary for your platform from the
same Releases page and run `centauri desktop` in a terminal — everything
below works the same.

## First run — what to expect

Click **Centauri** in the Start Menu. Two things happen:

1. **A black window opens.** That's the engine. It's normal — keep it open
   while you use Centauri. Closing it stops Centauri (your data is safe;
   it's saved to disk the moment you add it).
2. **Your browser opens** the app at `http://localhost:7771/app`.
   "localhost" means *this computer* — the page is served by the black
   window, not by a website.

The first time, the black window asks:

> Set up your private local AI now? [Y/n]

Press **Enter** (that means yes). Centauri then installs the one missing
piece it needs (a free local model runner called Ollama) and downloads the
AI models. **This download is a few gigabytes and happens once** — on a
typical connection it takes a few minutes to half an hour. You can keep
using the app meanwhile; adding data works immediately.

**How to tell it's ready:** the black window prints download progress, and
when it finishes it says:

> ai: your private AI is ready — ASK and SEARCH now run on local models,
> nothing leaves this machine.

If you ask a question before that line appears, you may get "I don't have a
model yet"-style answers — just wait for the downloads and ask again.

Next time you start Centauri there's nothing to download; it's ready in
seconds.

## Ask it things

The app has three tabs: **Ask**, **Add**, **Recent**.

On the **Ask** tab, type a question the way you'd say it out loud:

- *"Which notes mention Acme?"*
- *"Summarize what I added about pricing."*
- *"What did we agree with the landlord about the deposit?"*

Centauri finds the relevant items you've added, writes an answer, and tells
you how many of your items it was based on. If it doesn't know, it says so —
it only answers from what you've put in, which is exactly why you can trust
it.

## Put your data in

Honest note first: today, getting data in is mostly *typing or pasting*.
There's no drag-and-drop spreadsheet import yet.

- **The Add tab (easiest).** Type or paste anything — a customer detail, a
  phone call summary, contract terms, a paragraph from an email. Give it a
  title if you like, click **Add it**, and it's instantly saved and
  searchable.
- **Images and PDFs.** Open the [advanced dashboard](http://localhost:7771)
  (the link is at the bottom of the app) and click **📎 Vision** to upload
  an image or PDF. A local AI reads it and makes it searchable. This needs
  the local AI set up (see above) plus a PDF renderer — run
  `run-centauri.bat vision` once if PDF uploads complain.
- **Spreadsheets (needs a technical person).** If you or someone you know
  writes a little Python, the [Python SDK](../sdk/python) bulk-loads a CSV
  file in three lines: `db.pump("customers.csv")`.
- **Sample data to play with.** Run `run-centauri.bat seed` to fill the
  database with example retail data and see what asking questions feels
  like.

## Optional: cloud boost (GLM-5.2)

Out of the box Centauri is **100% local** — that's the point. But if you
want stronger answers than your computer's hardware can produce, you can
optionally connect the cloud model **GLM-5.2** from [Z.ai](https://z.ai):

1. Create a Z.ai account and get an **API key** (a long code that acts as
   your password to their service — treat it like one).
2. Open the Centauri dashboard's **AI panel** and paste the key in. One
   paste — done.

Be clear-eyed about the trade:

- **This is OFF by default.** Nothing goes to the cloud unless you paste a
  key.
- **When on, your questions and the relevant snippets of your data are sent
  to Z.ai's servers** to produce the answer.
- **It costs money** — Z.ai bills your account per use.

Remove the key in the same panel to go back to fully local at any time.

## Prefer separate switches?

One file does everything, but each piece also has its own double-clickable file:

- `start-all.bat` — everything: database + AI + browser (same as run-centauri.bat)
- `start-db.bat` — just the database (dashboard, Studio and API — no AI)
- `start-ai.bat` — switch the private local AI on for a running database (no API key needed — the local AI never uses one)
- `open-dashboard.bat` / `open-studio.bat` — open that screen (starts the engine first if needed)
- `stop-centauri.bat` — stop the engine

## If something goes wrong

**The browser didn't open.** Open it yourself and go to
`http://localhost:7771/app`.

**"Ollama" errors, or AI setup failed.** Ollama is the free local model
runner Centauri uses. If the automatic install fails (some networks block
it), install it yourself from [ollama.com/download](https://ollama.com/download),
then close and restart Centauri — it picks it up automatically.

**I said "no" to AI setup and want it now.** Delete the file
`.centauri-ai-optout` in the `%APPDATA%\Centauri` folder and restart
Centauri, and it will ask again.

**"Port busy" / "address already in use".** Something is already using port
7771 — usually another copy of Centauri. Run `run-centauri.bat stop` (or
restart the computer), then start Centauri again.

**Where is my data, actually?** One file:
`%APPDATA%\Centauri\centauri.log` — that is, inside your user folder at
`C:\Users\<you>\AppData\Roaming\Centauri\`. Copy that file and you've backed
up everything. Uninstalling Centauri does not delete it.

**It stopped and the window closed too fast to read.** Start it from
`run-centauri.bat` — that window stays open after an error so you can read
the message.

---

Want the full story — time travel, audit trails, the query language, the
Genesis Engine? Head back to the [README](../README.md) or open the built-in
textbook at `http://localhost:7771/ceql`.
