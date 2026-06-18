# Design: offline-first bidirectional sync

**Status:** record-level pull **and** deterministic multi-master convergence
built; push-mode/pairing and the browser replica remain · **Companion to:**
[design-own-your-data.md](design-own-your-data.md)

The "own your data" story promises data that lives on your devices and
reconciles across them. Today Centauri has the pieces — one-way `follow`
(primary → read replica), offline `merge` (union two diverged logs), the
single-writer lock for synced folders, and CDC (`/v1/changes`) + replication
slots. This doc designs the missing capability: **live, offline-first
bidirectional sync between writable replicas**, à la CouchDB/PouchDB — and is
honest about the one genuinely hard part.

## 1. Why facts make sync easy (mostly)

CouchDB/PouchDB sync is built on a `_changes` feed + a per-peer **checkpoint**
of progress, with conflicts handled via revision trees and a deterministic
winner. Centauri already has the feed (`/v1/changes`) and the checkpoint
(**replication slots**). And because Centauri facts are **immutable,
time-stamped, and globally-unique-id'd**, record-level sync is **conflict-free
by union**: two replicas just exchange the facts the other is missing.

The id makes it **echo-safe**: a fact that syncs A→B and would echo B→A is
recognized by its event id on the way back and **skipped**. No infinite loop,
no duplicate, no revision tree.

## 2. What's built (the safe slice)

- **`store.IngestForeign(events)`** — append facts received from a peer,
  **skipping any whose event id we already hold** (dedup = echo protection),
  **preserving each event's original id and `recorded_time`** so bi-temporal
  reads stay correct across replicas. It reuses the verified `IngestRaw`
  write/apply/chain path.
- **`centauri sync -primary <peer>`** — a loop that pulls the peer's
  `/v1/changes` from a per-peer **slot** cursor, calls `IngestForeign`, and
  advances the slot. Run it on **both** nodes pointing at each other for
  bidirectional sync; id-dedup keeps it from echoing.

This is **correct and convergent** for the common cases:
- **disjoint subject namespaces** (device A writes `deviceA:*`, B writes
  `deviceB:*`), and
- **single-origin streams** (each subject has one writer; supersession arrives
  in `recorded_time` order, so the latest fact wins the current pointer).

Bi-temporal reads stay correct because `AS OF` / `AS KNOWN AT` select by the
events' own `effective_time`/`recorded_time` (which we preserve), not by log
order — see `asOfLocked`.

## 3. Multi-master on the *same* subject (built)

Centauri's *current-fact* pointer (`open`) used to be set in `apply()` by
**last-applied-wins**, which is **log-order dependent**: if two replicas each
accepted an independent write on the *same* `(subject, facet)` and then synced,
they applied the two facts in different orders → they could disagree on which is
"current." (CouchDB hits the same problem and resolves it with a deterministic
winner by revision.)

**Fix (now in `store.apply`):** "current" is a **deterministic function of the
facts**, independent of apply order — the non-superseded fact with the greatest:

1. `effective_time`, then
2. `recorded_time`, then
3. `event_id` (lexicographic — a stable, replica-independent tiebreaker).

This is a last-write-wins register keyed on (effective, recorded, id) — the same
idea as Cassandra's per-cell timestamp resolution. It's implemented as `beats(a,
b)` plus an incremental max on each event apply, and a `recomputeOpen(k)` on
supersession (so removing a fact from eligibility restores the next-best — e.g.
a correction that carries an *earlier* effective time than the fact it replaces
still becomes current, because the prior is explicitly superseded). Every
replica computes the same current fact from the same set of facts, regardless of
ingest order → **convergence**, and replay is deterministic by construction.
Tests: `internal/store/deterministic_test.go` (shuffled-order convergence,
id tiebreak, earlier-effective correction).

Note also that supersession **markers** are not carried by `/v1/changes` (it
emits fact events, not the internal supersede records). For single-origin
streams this is fine (latest `recorded_time` wins the pointer); the
deterministic-winner rule removes the dependency on markers entirely for the
multi-master case.

## 4. Integrity across replicas

The tamper-evident hash chain is computed over **local log order**. Two replicas
that ingest the same facts in different orders therefore have **different but
each-valid** chains over their own logs — this is expected and fine (every node
can prove its own history). A single canonical chain is produced by `merge`,
which unions records and re-chains deterministically. Sync does not promise
byte-identical logs; it promises the **same set of facts**, with each replica's
reads converging (after §3).

## 5. Relationship to existing tools

| Tool | Role |
|------|------|
| `follow` | one-way, byte-level, chain-identical replica (read-only follower). |
| `sync` | bidirectional, record-level, echo-safe (this doc). |
| `merge` | offline union of two diverged logs into a fresh canonical log. |
| replication **slots** | per-peer progress checkpoint (CouchDB's replication checkpoint). |
| `/v1/changes` | the change feed (CouchDB's `_changes`). |

## 6. Build order

1. **`IngestForeign` + `centauri sync`** (done) — safe for disjoint/single-origin.
2. **Deterministic current-fact rule** (§3) — **done**: unlocks correct
   multi-master on shared subjects; convergence tested under shuffled orders.
3. **Push mode / pairing** — optional: a single `sync` that both pulls and asks
   the peer to pull (today: run `sync` on both ends).
4. **Browser/edge replica** — a small client (on the JS SDK) that keeps a local
   cache and syncs — the PouchDB end-state.

## 7. Prior art

- **CouchDB / PouchDB** — `_changes` feed + replication checkpoints +
  offline-first bidirectional sync; deterministic conflict winner.
- **CRDTs** — immutable, mergeable facts are a coarse CRDT; the deterministic
  max-rule (§3) is a last-writer-wins register keyed on (effective, recorded, id).
