// Centauri JS/TS client — zero-dependency, fetch-based. Works in Node 18+
// and the browser. Mirrors the Python and Go SDKs: one method per capability.
//
//   import { Centauri } from "./centauri.mjs";
//   const db = new Centauri("http://localhost:7771", { token: "secret" });
//   await db.add("toy:robot", { price_cents: 500 });
//   const facts = await db.get("toy:robot");
//   const res   = await db.query("FACTS OF toy:robot WHY");
//   await db.run("reprice", { item: "toy:robot", pct: 90 });

export class CentauriError extends Error {}

export class Centauri {
  constructor(url = "http://localhost:7771", { token = "", database = "" } = {}) {
    this.url = url.replace(/\/$/, "");
    this.token = token;
    this.database = database;
  }

  _qs(extra = {}) {
    const p = new URLSearchParams(extra);
    if (this.database) p.set("db", this.database);
    const s = p.toString();
    return s ? "?" + s : "";
  }

  async _req(method, path, { query = {}, body = null } = {}) {
    const headers = {};
    if (body != null) headers["Content-Type"] = "application/json";
    if (this.token) headers["Authorization"] = "Bearer " + this.token;
    const resp = await fetch(this.url + path + this._qs(query), {
      method, headers, body: body != null ? JSON.stringify(body) : undefined,
    });
    const text = await resp.text();
    const data = text ? JSON.parse(text) : null;
    if (!resp.ok) throw new CentauriError((data && data.error) || `HTTP ${resp.status}`);
    return data;
  }

  // any CeQL — the escape hatch
  query(ceql) { return this._req("POST", "/v1/query", { body: { q: ceql } }); }

  // write (insert and update are the same act)
  append(events) {
    return this._req("POST", "/v1/append", { body: { events } })
      .then((r) => (r && r.appended) || []);
  }
  async add(subject, value, opts = {}) {
    const ids = await this.append([{
      subject, facet: opts.facet || "source", type: opts.type || "OBSERVED",
      value, confidence: opts.confidence ?? 1,
      provenance: opts.provenance || "HUMAN_ENTRY", source_system: "js-sdk",
    }]);
    return ids[0];
  }

  // read
  get(subject) { return this._req("GET", "/v1/current", { query: { subject } }); }
  history(subject) { return this._req("GET", "/v1/history", { query: { subject } }); }
  at(subject, at, known) {
    const q = { subject };
    if (at) q.at = String(at);
    if (known) q.known = String(known);
    return this._req("GET", "/v1/asof", { query: q });
  }
  context(subject) { return this._req("GET", "/v1/context", { query: { subject } }); }
  stats() { return this._req("GET", "/v1/stats"); }
  subjects() { return this._req("GET", "/v1/subjects"); }

  // CePL procedures
  defineProcedure(source) { return this._req("POST", "/v1/proc", { body: { source } }); }
  run(name, args = {}) { return this._req("POST", "/v1/proc/run", { body: { name, args } }); }

  // change data capture (resumable)
  changes(from = 0) { return this._req("GET", "/v1/changes", { query: { from: String(from) } }); }
}

export default Centauri;
