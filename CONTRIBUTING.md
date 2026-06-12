# Contributing to Centauri

Suggestions, bug reports, and pull requests are all welcome — from humans
and from AI working with humans.

## The automated path (suggestion → Claude → PR)

1. **Open an issue** with the Feature request or Bug report template.
   The more concrete the description, the better the result — the templates
   ask exactly what's needed.
2. **A maintainer triages it.** If accepted, they comment `@claude` on the
   issue with any extra guidance (e.g. "implement this; follow the pattern
   in internal/ceql; add tests").
3. **Claude implements it** in a branch and opens a pull request, with
   tests. This runs on GitHub Actions via the official
   [claude-code-action](https://github.com/anthropics/claude-code-action).
4. **CI gates everything:** `go vet` and `go test ./...` must pass, plus the
   Python SDK tests. No green, no merge.
5. **A human merges.** Always. Automation writes code; people own the
   database.

## The classic path

Fork, branch, code, `go test ./...`, PR. Style: standard `gofmt`; zero
third-party Go dependencies is a deliberate feature of this codebase —
PRs adding dependencies need a very good reason.

## Ground rules for changes

- **Nothing is ever erased** is the product. Any change that mutates or
  deletes committed history will be rejected.
- Every replay-visible state change must happen in `store.apply()` so
  logs replay identically (see the comment on that function).
- New CeQL statements need: parser + executor + a textbook section in
  `internal/api/ceql.html` + a catalog entry + tests.
- New features need a dashboard surface. If users can't see it, it
  doesn't exist.
- Honest docs: capability tables in this project state what we *don't*
  do. Keep it that way.

## Running tests

```
go vet ./... && go test ./...
cd sdk/python && python -m unittest discover -s tests
```
