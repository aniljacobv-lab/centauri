# Connect cloud AI assistants (Claude, Claude Duet, Meta Muse)

`centauri serve` answers the Model Context Protocol over HTTPS at **`/mcp`**
(Streamable HTTP, MCP revisions 2025-06-18, 2025-03-26 and 2024-11-05). Cloud
assistants that take a custom connector by URL can then read Centauri with the
same 20 tools `centauri mcp` gives a local agent: context, current facts, time
travel (`AS OF`, `AS KNOWN AT`), causal trace, disagreements, pending wedges,
similarity search and CeQL.

For an assistant on the same computer (Claude Desktop, Claude Code), the stdio
server is still the simplest: `centauri mcp -data centauri.log`.

## Before you connect: what leaves the machine

Centauri's promise is that your data stays on your machine. A cloud assistant
breaks that for whatever it reads: the facts it fetches go to that assistant's
cloud (Anthropic's for Claude, Meta's for Muse) under that provider's terms.
So:

- Give assistants the **read token**, never the admin token. With it they see
  only the tools that read, and every call that could append is refused: the
  write tools, CeQL `PUT`/`CORRECT`/`RETIRE`/`DEFINE`/`RUN`/`SNAPSHOT`/`ROLLBACK`,
  stored procedures, and a Genesis blueprint asked to build itself.
- Serve a database that holds only what you mean to share (`-data shared.log`),
  or use `?db=` with a named environment, rather than your whole log.
- With **no tokens configured**, `/mcp` answers only `localhost`, so a
  tokenless server behind a public URL is not an open door.

## 1. Serve Centauri on a public HTTPS address

```
centauri serve -data shared.log -token "$ADMIN_TOKEN" -read-token "$READ_TOKEN"
```

Put it behind HTTPS: the Render blueprint in `render.yaml`, any reverse proxy
with a certificate, or a tunnel. Assistants refuse plain `http://` addresses.

Check it from anywhere:

```
curl -s https://YOUR-HOST/mcp -H "Authorization: Bearer $READ_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## 2. Add the connector

Two URL forms work. Use the header form wherever the assistant lets you set a
header; use the token-in-path form for an assistant that only takes a URL
(the token is kept out of Centauri's request log as `/mcp/[redacted]`, but
treat that URL as a secret).

| Form | URL |
|---|---|
| Header auth | `https://YOUR-HOST/mcp` with `Authorization: Bearer <read-token>`, or `X-API-Key: <read-token>` |
| URL only | `https://YOUR-HOST/mcp/<read-token>` |

**Claude (including Claude Duet):** Settings → Connectors → Add custom
connector → name it Centauri and paste `https://YOUR-HOST/mcp`, choose **No
sign-in**, and under Request headers add `X-API-Key` with the read token as its
value (Claude may keep `Authorization` for its own sign-in). The URL-only form
works too. Claude Duet uses the same
connectors as Claude, and asks before any sensitive action.

**Meta Muse:** Settings → Connectors → Add custom connector → paste the URL.
Listing Centauri in Muse's public directory is a separate review at
muse.ai/platform.

Then ask the assistant something like *"Using Centauri, what did we believe
the price of item 100001 was on March 3, and why did it change?"*

## What the endpoint does and does not do

- Stateless: each POST carries one JSON-RPC message (or a batch) and is
  answered as `application/json`. There is no session id and no server-sent
  event stream, so `GET /mcp` answers 405, as the spec allows.
- A browser page on another origin is refused (the spec's DNS-rebinding
  guard); server-to-server clients send no `Origin` and pass.
- Auth is the API's: static tokens, read tokens and enterprise SSO (OIDC)
  JWTs. Centauri does **not** run an OAuth authorization server, so an
  assistant that insists on an OAuth sign-in flow cannot connect yet; use the
  URL form. Scoped (row-level) tokens are confined to `/v1/query` and cannot
  use `/mcp`.
- No resources or prompts: tools only, as over stdio.
