# The Gentlest Centauri Tutorial

*No databases knowledge needed. No terminal skills needed. Just Python.*

## What is Centauri? (one minute)

Imagine a notebook that **never erases anything**.

When the price of a toy changes, you don't scribble out the old price —
you write the new price on a new line, with the date. Now you can always
answer two magic questions:

1. **"What's the price now?"** — read the newest line.
2. **"What was the price on my birthday?"** — read the line that was
   newest back then.

Centauri is that notebook, made fast. Plus three superpowers:

- Every fact remembers **when it became true** AND **when you learned it**
  (those can be different — you learn about price changes late sometimes!).
- Every fact remembers **what caused it** ("the price dropped *because*
  head office sent change #123").
- Every fact remembers **where it came from and how much to trust it**.

That's why AI tools love it: they can ask one question and get the whole
truthful story, not just the latest number.

## Step 1 — Get ready (once)

```bash
cd sdk/python
pip install -e .
```

## Step 2 — Start the database

```python
from centauri import Centauri

db = Centauri.launch()
```

That's the whole setup. `launch()` finds the Centauri server program,
starts it, and connects. (If a server is already running, it just
connects to it.)

> Stuck? The error message tells you exactly what to do. Really — read
> it, it was written for you.

## Step 3 — Save your first fact

```python
db.add("toy:robot", {"price_cents": 500})
```

You just said: *"the robot toy costs 500 cents — true from now on."*

## Step 4 — Change your mind (this is the fun part)

```python
db.add("toy:robot", {"price_cents": 450})
```

In a normal database, the 500 would be gone forever. In Centauri,
**both** facts exist: the 450 is "current", the 500 is "history".

```python
db.get("toy:robot")          # -> [Event(... value={'price_cents': 450} ...)]
db.history("toy:robot")      # -> BOTH events, in time order
```

## Step 5 — Time travel

```python
db.at("toy:robot", "2026-06-01")    # what was true on June 1st?
```

And the double-time-travel trick that no ordinary database can do:

```python
db.at("toy:robot", "2026-06-01", known="2026-05-15")
```

*"What did we BELIEVE on May 15th about June 1st?"* — perfect for
answering "why did the system make that decision back then?"

## Step 6 — Load lots of data at once

Got a spreadsheet? Save it as CSV with a `subject` column, then:

```python
db.pump("my_prices.csv")
```

Done. Each row became a fact.

## Step 7 — Watch facts arrive, live

```python
for event in db.watch():
    print("Something new!", event.subject, event.value)
```

Leave this running in one window and `db.add(...)` from another. Magic.

## Step 8 — The one-question-everything query

```python
ctx = db.context("toy:robot")
print(ctx.facts)            # what's true now
print(ctx.history)          # how it got that way
print(ctx.disagreements)    # any parts of the system that disagree
print(ctx.pending)          # promised work that never finished
```

This bundle is what you hand to an AI (or a colleague) when you want
them to *understand* the robot toy, not just look it up.

## Step 9 — Tidy up

```python
db.stop()    # stops the server launch() started
```

Or use a `with` block and it happens automatically:

```python
with Centauri.launch() as db:
    db.add("toy:robot", {"price_cents": 425})
```

## Where next?

- `examples/` — five small scripts you can run right now.
- `README.md` — the full menu of what the SDK can do.
- Ask the database itself! `db.stats()`, `db.subjects()` — explore.
