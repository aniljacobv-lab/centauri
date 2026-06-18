# Centauri JS/TS SDK

Zero-dependency, `fetch`-based client for Centauri. Node 18+ and the browser.

```js
import { Centauri } from "@centauri/client"; // or "./centauri.mjs"

const db = new Centauri("http://localhost:7771", { token: "secret" });

await db.add("toy:robot", { price_cents: 500 });   // insert == update
const facts  = await db.get("toy:robot");           // what's true now
const hist   = await db.history("toy:robot");       // full timeline
const bundle = await db.context("toy:robot");       // everything, for an agent

const res = await db.query("FACTS OF item:* WHERE region='EU' WHY"); // any CeQL

// CePL procedures
await db.defineProcedure(`PROCEDURE reprice(item, pct)
  LET cur = FIRST FACTS OF \${item}
  WHEN cur IS MISSING: FAIL 'unknown \${item}'
  LET newp = cur.price_cents * pct / 100
  PUT \${item} SET price_cents=\${newp}
  RETURN newp
END`);
await db.run("reprice", { item: "toy:robot", pct: 90 });

// change data capture
const { events, cursor, caught_up } = await db.changes(0);
```

Options: `{ token, database }`. `query()` runs any CeQL the SDK hasn't wrapped.
