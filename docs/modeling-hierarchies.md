# Modeling Hierarchies in Centauri

*The worked example: a retail merchandise + location hierarchy with
SKU-at-location status — and how integrity works without foreign keys.*

## The shape

Merchandise: company → chain → group → department → class → subclass →
item parent → item variant → SKU.
Location: store/warehouse → district → region → area/country → company.
Transactional leaf: SKU × location, with units/cost/price held by several
systems that may disagree.

## Principle 1: nodes are subjects, edges are facts

Every hierarchy node is a subject; its parent pointer is an ordinary fact:

    PUT company:1        SET name='Proxima Retail'
    PUT chain:10         SET name='Outlet',     parent='company:1'
    PUT group:32         SET name='Apparel',    parent='chain:10'
    PUT dept:320         SET name='Mens Denim', parent='group:32'
    PUT class:3204       SET name='Slim Fit',   parent='dept:320'
    PUT subclass:320401  SET name='Stretch',    parent='class:3204'
    PUT item:100001      SET name='5-pocket jean', parent='subclass:320401', level='parent'
    PUT item:100001-RED  SET color='RED',  parent='item:100001',    level='variant'
    PUT sku:100001-RED-32 SET size='32',   parent='item:100001-RED', level='sku'

    PUT store:4001    SET name='Houston Galleria', parent='district:41', type='store'
    PUT district:41   SET parent='region:4'
    PUT region:4      SET parent='country:US'
    PUT country:US    SET parent='company:1'

Because edges are facts, **the hierarchy is bi-temporal**:

    FACTS OF class:3204 AS OF '2026-01-15'     -- which dept was it in, in January?
    HISTORY OF class:3204                      -- every reclass ever
    FACTS OF class:3204 WHY                    -- what caused the latest reclass

A reclass is one PUT. The old structure is never lost; January reports
re-run under January's hierarchy via AS OF / AS KNOWN AT.

## Principle 2: denormalize ancestry onto the leaves (it's versioned anyway)

For instant roll-ups, write the ancestor chain onto transactional facts —
the same thing RMS does in item_master, except here it's superseding facts,
so the denormalization has history too:

    PUT sku:100001-RED-32/store:4001 FACET source
      SET units=12, cost_cents=900, price_cents=1999,
          dept='dept:320', class='class:3204', region='region:4'
      SCHEMA sku_loc_status

Roll-ups become one query, and they time-travel:

    FACTS dept, SUM(units), AVG(price_cents) OF sku:* GROUP BY dept
    FACTS region, SUM(units) OF sku:* GROUP BY region AS KNOWN AT '2026-03-01'

A reclass procedure (below) re-stamps the denormalized fields; the old
stamps remain in history, which is exactly what an auditor wants.

## Principle 3: facets carry each system's belief about the leaf

    PUT sku:100001-RED-32/store:4001 FACET register SET price_cents=1999
    PUT sku:100001-RED-32/store:4001 FACET pdt      SET price_cents=1899

Then price integrity is a query, not a reconciliation batch:

    DISAGREE ON price_cents               -- where do systems disagree right now?
    PENDING pdt OLDER THAN 21 DAYS        -- sent to PDT, never activated
    WATCH ALL FACET pdt TYPE DISTRIBUTED  -- agents react instead of polling

## Principle 4: integrity = schemas + procedure gateways (not FKs)

Honest statement: Centauri has no foreign-key constraints. A raw PUT can
name a parent that doesn't exist. The integrity pattern is the one mature
retail systems already use — nobody inserts into item_master directly;
writes go through an API. In Centauri the API layer lives *inside* the
database as CePL procedures:

    PROCEDURE record_sku(id, variant, size, dept)
      LET v = FIRST FACTS OF ${variant}
      WHEN v IS MISSING: FAIL 'parent variant ${variant} does not exist'
      LET d = FIRST FACTS OF ${dept}
      WHEN d IS MISSING: FAIL 'department ${dept} does not exist'
      PUT sku:${id} SET parent='${variant}', size='${size}', dept='${dept}' SCHEMA sku REF 'proc:record_sku'
      RETURN id
    END

    PROCEDURE reclass_class(class, new_dept)
      LET d = FIRST FACTS OF ${new_dept}
      WHEN d IS MISSING: FAIL 'department ${new_dept} does not exist'
      PUT ${class} SET parent='${new_dept}' REF 'proc:reclass_class'
      RETURN new_dept
    END

What you get that FKs never gave you:

- **Versioned validation** — schemas reject bad shapes/ranges; schema
  changes never break old facts.
- **Lineage on every accepted write** (REF + WHY) — each fact knows which
  procedure admitted it.
- **Tamper-evidence** — the hash chain proves nobody edited history.
- **Semantic integrity** — DISAGREE and PENDING catch the failures FKs
  can't see: systems drifting apart, changes that never landed.
- Orphan sweeps: run a periodic agent/script (or dream-cycle check) that
  walks parents and files issues for dangling references.

Declared referential types in schemas (e.g. `parent ref(item)`) are on the
ROADMAP — until then, the procedure gateway is the rule: **applications
call procedures; only admins use raw PUT.**

## Building future systems on this

1. **Procedures are your governed API.** Like RMS packages, but versioned,
   self-tracing, and queryable (`HISTORY OF proc:record_sku`).
2. **Agents work the exception queue.** WATCH + PENDING turn nightly
   reconciliation batches into continuous, accountable repair — every fix
   an agent writes carries provenance AI_INFERRED and a confidence.
3. **One-call context for AI assistants.** `CONTEXT FOR sku:X/store:Y`
   is the complete brief: state, history, causes, disagreements, trust.
4. **Audits are queries.** `AS KNOWN AT` re-creates any past decision
   context; `centauri verify` proves the record untampered.
5. **Warehouse downstream.** `centauri export -q "FACTS OF sku:*" -format csv`
   feeds Snowflake/BI for heavy analytics; Centauri remains the system of
   record for what happened, when, why, and how much to trust it.
