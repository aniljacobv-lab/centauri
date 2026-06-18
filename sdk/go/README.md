# Centauri Go SDK

A thin, **zero-dependency** Go client (stdlib `net/http` + `encoding/json`) for a
Centauri server. Same surface as the Python SDK — one method per capability.

```go
import centauri "github.com/proxima360/centauri/sdk/go"

c := centauri.New("http://localhost:7771", centauri.WithToken("secret"))

// write (insert and update are the same act)
c.Add("toy:robot", map[string]any{"price_cents": 500})

// read
facts, _   := c.Get("toy:robot")          // what's true now
hist, _    := c.History("toy:robot")      // the full timeline
past, _    := c.At("toy:robot", atMicros, knownMicros) // time travel
bundle, _  := c.Context("toy:robot")      // everything, for an agent

// any CeQL — the escape hatch
res, _ := c.Query("FACTS OF item:* WHERE region='EU' WHY")

// CePL procedures
c.DefineProcedure(`PROCEDURE reprice(item, pct)
  LET cur = FIRST FACTS OF ${item}
  WHEN cur IS MISSING: FAIL 'unknown ${item}'
  LET newp = cur.price_cents * pct / 100
  PUT ${item} SET price_cents=${newp}
  RETURN newp
END`)
c.Run("reprice", map[string]any{"item": "toy:robot", "pct": 90})

// change data capture (resumable)
evs, cursor, caughtUp, _ := c.Changes(0)
```

Options: `WithToken`, `WithDatabase` (named environment / `?db=`), `WithHTTPClient`.
For anything the SDK hasn't wrapped yet, `Query` runs arbitrary CeQL.
