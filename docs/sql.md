# Lean SQL — a familiar front door

Centauri's full query language is CeQL, but for people (and LLMs) who already think
in `SELECT`, there is a **lean read-only SQL subset** that transpiles to CeQL and
runs through the same executor. It is meant to lower the "do I have to learn a new
language?" barrier — not to be a SQL wire protocol.

```
GET  /v1/sql?q=SELECT * FROM sku WHERE category='beverage' LIMIT 10
POST /v1/sql   {"q": "SELECT category, COUNT(*) FROM facts GROUP BY category"}
```

## What's supported

- `SELECT * | col, col, AGG(col)` — `COUNT`, `SUM`, `AVG`, `MIN`, `MAX` (and `AS alias`, which is accepted and ignored).
- `FROM <name>` → the namespace `<name>:*`; `FROM facts` (or `events`) → all subjects; quote an exact subject or pattern: `FROM 'item:1'`, `FROM 'sku:*'`.
- `WHERE` — `=`, `!=`/`<>`, `<`, `<=`, `>`, `>=`, `IN (...)`, `LIKE`, combined with `AND` / `OR` / `NOT` and parentheses. A top-level `subject = '…'` or `facet = '…'` is lifted onto the query.
- `GROUP BY col`, `HAVING AGG(col) <op> n`, `ORDER BY col [ASC|DESC]`, `LIMIT n [OFFSET m]`.
- Time travel: `AS OF <when>` (valid time) and `AS KNOWN AT <when>` (transaction time). SQL:2011 `FOR SYSTEM_TIME AS OF <when>` is accepted as a synonym for `AS KNOWN AT`. `<when>` is a quoted date/natural-time string (`'2026-03-15'`, `'yesterday'`) or raw UnixMicros.

```sql
SELECT * FROM sku WHERE category = 'beverage' AND price_cents > 1000 ORDER BY price_cents DESC LIMIT 5
SELECT category, AVG(price_cents) FROM facts GROUP BY category HAVING COUNT(*) > 3
SELECT * FROM 'item:1' FOR SYSTEM_TIME AS OF 'yesterday'
```

## What it is not

- **Not a SQL wire protocol.** There is no JDBC/ODBC/pgwire driver, so BI tools
  (Tableau, Power BI, DBeaver) cannot connect directly — they would need a wire
  protocol + catalog, which is a separate, much larger effort.
- **Read-only.** Only `SELECT` is accepted; writes (PUT/CORRECT/RETIRE) use CeQL.
- **A subset, on purpose.** No joins, subqueries, window functions, or `COUNT(DISTINCT)`
  yet. It rejects what it doesn't support rather than silently mis-answering.

CeQL remains the complete surface (time, cause, trust, search, topology); lean SQL is
just an approachable door onto the current-state slice of it. See `/ceql` for the
full CeQL textbook.
