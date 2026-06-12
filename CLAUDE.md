# Centauri — guide for AI contributors

You are working on Centauri: a bi-temporal, causal, AI-first event database
in Go. One binary, zero third-party Go dependencies (stdlib only — keep it
that way). Read this before changing anything.

## Map

- `internal/model`    — Event/Schema/Enrichment/CausalLink types. Events are immutable.
- `internal/store`    — append-only JSONL log + in-memory indexes. THE critical package.
  - `store.go` commit/replay/Append/AsOf; `checkpoint.go`; `integrity.go` (hash chain);
    `ship.go` (replication); `schema.go`; `search.go` (vectors); `context.go`; `watch.go`.
- `internal/ceql`     — the query language: `ceql.go` (lexer/parser/AST), `exec.go`,
  `natural.go` (natural time + NL→CeQL rules).
- `internal/proc`     — CePL stored procedures (parser + interpreter).
- `internal/architect`— the Genesis Engine (scenario → interview → blueprint).
- `internal/catalog`  — command catalog seeded into the ceql-catalog environment.
- `internal/api`      — HTTP server; `ui.html` (dashboard) and `ceql.html` (textbook)
  are EMBEDDED via go:embed — changes require rebuild.
- `internal/mcp`      — MCP stdio server for agents.
- `cmd/centauri`      — main: serve/seed/mcp/follow/verify/backup.
- `sdk/python`        — zero-dependency Python SDK + mock-server tests.

## Invariants (violating these = rejected PR)

1. **Nothing is ever erased.** No code path may mutate or delete committed
   log bytes. "Update" = append a superseding fact. "Delete" = RETIRE.
2. **Replay determinism:** every replay-visible state change goes through
   `store.apply()`. State set anywhere else is lost on restart. If you add
   index state, also add it to the checkpoint (checkpoint.go) and reset it
   in `resetState()`.
3. **Write-then-apply:** memory changes only after bytes are durably
   written (see `commit()`); crash-ordering: events before their
   supersession markers.
4. **The hash chain** (`integrity.go`) covers every committed line in
   commit/replay/IngestRaw identically — if you touch the write path,
   keep all three in sync and update the chain tests.
5. **Zero Go dependencies.** The empty `require` block in go.mod is a feature.

## Feature checklist

A new CeQL capability is complete only with: parser + executor + tests in
`internal/ceql`, a section in `internal/api/ceql.html` (the textbook), an
entry in `internal/catalog/catalog.go` (autocomplete data — examples must
parse; the catalog test enforces this), and — if user-facing — a dashboard
surface in `internal/api/ui.html`. Features without a dashboard surface
don't exist for users.

In ui.html JS: when embedding values in inline handlers use `jsAttr(...)`
(never `esc(...)` inside single quotes — apostrophe bug), and `loadJSON`
for localStorage reads.

## Verify

```
go vet ./... && go test ./...
cd sdk/python && python -m unittest discover -s tests
```

Run both before opening a PR. Documentation honesty is policy: comparison
tables state what Centauri does NOT do; never inflate a ✗ to a ✓.
