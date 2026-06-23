# Centauri Enterprise Deployment Gaps Review (Rerun)

**Date:** 2026-06-23  
**Reviewed code state:** Post-enterprise updates including native TLS (`-tls-cert`/`-tls-key`), auth on lazy read path, Prometheus `/metrics`, `/livez` `/readyz` probes, admission control (`-max-concurrency`, `-query-timeout`) via `WithLimits`. Focus on `serve -lazy-index` + `LazyIndex` / tablespaces read path, integration with normal `serve`, and updated `docs/enterprise-readiness.md`.  
**Process:** Read key files (cmd/centauri/main.go full, internal/api/lazy.go, limits.go, lazy_test.go, api.go chunks for Routes/auth, limits_test.go, store/lazyindex.go + archive_reader.go + scan.go, enterprise-readiness.md, enterprise-deployment-gaps.md, lazy.html, deploy.md, production-suggestions.md). Used grep across paths for TLS/auth/limits/health/metrics. No code changes. Citations use absolute paths + line numbers. Compared to prior gaps (2026-06-18).  

Documentation honesty policy (per CLAUDE.md) observed: claims matched to actual implementation (e.g., lazy-only features not generalized).

---

## Summary

**Overall assessment:** The recent enterprise updates represent targeted, honest progress focused on the **lazy-index read path** (`serve -lazy-index`). 

- **Strengths added:**
  - Native TLS support now works for both normal `serve`/`desktop` and `-lazy-index` (via shared `listenMaybeTLS`).
  - Auth gap closed for lazy: `lazyAuth` now gates `/v1/*` data routes (using `-read-token` or `-token`).
  - Observability + K8s readiness added to lazy path: standard `/metrics` (Prometheus text format), bare `/livez`, manifest-aware `/readyz`.
  - Admission control added exclusively to lazy: `WithLimits` provides global concurrency (429) + per-query timeout (503) to protect cold scans.
  - Full lazy read path (`LazyIndex` + `archiveReader` + zone-map pruning + LRU segment cache + Merkle checkpoint) is implemented read-only and correctly separated from the full in-RAM `Server`.

- **Integration notes:** Lazy mode (`main.go:307 if cmd == "serve" && *lazyIndex`) is a completely separate branch: `store.OpenLazyIndex` + `api.LazyRoutes(li, tok)` + `WithLimits` + `listenMaybeTLS`. Normal `serve` uses `store.Open*` + `api.NewWithOptions` + `srv.Routes()` (no limits wrapper, no healthz/metrics endpoints). This is by design (lazy is read-only high-scale cold path), but creates asymmetry.

- **Documentation assessment (enterprise-readiness.md):** Mostly accurate and improved, but contains overstatements/omissions around observability and auth (see below). The matrix is duplicated live in the lazy dashboard (`internal/api/lazy.html`). Compared to `enterprise-deployment-gaps.md`, several "Security quick wins" and "Observability" and "Operational hardening" recommendations from the prior review are now implemented — but scoped only to `-lazy-index`.

**Verdict:** Good incremental hardening for the cold-tier scale read use case. The main/hot write path and normal `serve` remain largely unchanged and still exhibit the bulk of the enterprise gaps (single-writer, no write admission, no healthz on primary path, no structured logs, process-shared multi-tenancy, etc.). The readiness matrix slightly over-claims general availability of new features.

---

## Code Review - Enterprise Gaps

Remaining issues (post-updates), with severity, specific citations, and suggestions. Focus on enterprise concerns. No critical correctness bugs found in the exercised paths (tests pass; invariants in store respected).

### 1. Observability / Health / Metrics — Only on Lazy Path (High severity for ops)
- **Files:** `cmd/centauri/main.go:467` (normal serve), `427` (desktop), `483` (follow); `internal/api/api.go:198-247` (Routes — no health/metrics handlers); `internal/api/lazy.go:203-239` (only lazy mux registers `/livez`, `/readyz`, `/metrics`).
- **Description:** `/livez`, `/readyz`, and Prometheus `/metrics` (with lazy-specific gauges/counters for resident keys, segments, cache hits/misses/decompressions) exist **exclusively** under `serve -lazy-index`. Normal `serve` (the primary path for writes + full queries) exposes `/v1/stats`, `/v1/integrity`, etc., but no standard liveness/readiness or Prometheus endpoint. `main.go:334` prints the health/metrics URLs only in lazy block. In K8s, a normal serve pod has no ready probe target.
- **Suggestion:** Add equivalent (lightweight) `/livez`/`/readyz`/`/metrics` (exposing core store stats + version) to the normal `Server.Routes()` path or a common middleware. At minimum, document that these are lazy-only.
- **Enterprise impact:** Breaks standard Prometheus scrape + K8s probes for the main deployment mode. Operators must special-case `-lazy-index`.

### 2. Admission Control / Limits — Lazy-Only + Write Path Completely Open (High severity)
- **Files:** `cmd/centauri/main.go:338` (`handler := api.WithLimits(api.LazyRoutes(...) ...)` only in lazy branch); `internal/api/limits.go:17-36` (semaphore + `http.TimeoutHandler`); `internal/api/api.go:298` (only `write()` gate for read-only, no concurrency/timeout); `internal/store/store.go:581` (Append holds `s.mu.Lock()` with no admission).
- **Description:** `WithLimits` (timeout inner, then concurrency outer; 429 with Retry-After, 503 on timeout) is **only** wrapped for lazy. Normal serve write endpoints (`/v1/append` etc. via `s.write()`) and read queries (including LLM-heavy `ASK`/`enrich` and large `SEARCH`/`HISTORY`) have zero backpressure. `max-concurrency` and `query-timeout` flags are parsed unconditionally but ignored outside lazy. Cold scans + vision inference can still starve the process.
- **Suggestion:** Apply a (perhaps lighter) `WithLimits` or per-handler timeout to normal `Routes()` (or at least to heavy query paths). Add write-side simple concurrency cap or queue. Per-tenant (named DB via `?db=`) quotas remain absent.
- **Enterprise impact:** Noisy neighbor / SLA violation risk documented as "partial" but still fully present on the hot path.

### 3. TLS Implementation Is Basic (Medium severity)
- **Files:** `cmd/centauri/main.go:1241-1246`:
  ```go
  func listenMaybeTLS(addr, cert, key string, h http.Handler) error {
  	if cert != "" && key != "" {
  		return http.ListenAndServeTLS(addr, cert, key, h)
  	}
  	return http.ListenAndServe(addr, h)
  }
  ```
  Used at 339 (lazy), 427 (desktop), 467 (serve), 483 (follow). Flags at 85-86.
- **Description:** Simple stdlib TLS termination when both files provided. No mTLS, no cert reloading, no ALPN/h2 prioritization, no client cert auth, no SNI handling beyond std. Deploy.md still recommends reverse proxy and claims "speaks plain HTTP".
- **Suggestion:** Add warning logs or docs for production TLS (e.g., reload on SIGHUP, cert validation). Update deploy.md recipes. Consider optional mTLS later.
- **Enterprise impact:** Meets "no reverse proxy required" claim but lacks controls expected in regulated environments (scanners often want app-level certs + client auth).

### 4. Auth Coverage Inconsistencies Between Paths (Medium)
- **Files:** `internal/api/lazy.go:261` (`if strings.HasPrefix(r.URL.Path, "/v1/")`); `internal/api/api.go:242` (`root.Handle("/", s.auth(mux))`), `243` (`/v1/version` outside auth), `252-296` (auth logic with ReadToken + scoped ACLs).
- **Description:** 
  - Lazy: `lazyAuth` intentionally leaves `/`, `/livez`, `/readyz`, `/metrics` open; gates all `/v1/*` (including `/v1/version`).
  - Normal: auth on data mux; UI/ceql/studio/version explicitly unauthenticated (token entered client-side in dashboard). ACLs environment-scoped (improved vs. prior review).
  - Lazy uses only read token (main.go:323-326); normal supports distinct `-token` / `-read-token`.
- **Suggestion:** Align `/v1/version` behavior (expose unauthed in lazy too, or gate consistently). Document that dashboard endpoints are unauthenticated by design (they carry no data).
- **Enterprise impact:** Minor leakage of version/build info on authed lazy instances. Auth now present on lazy data (closes old gap), but the two server modes differ.

### 5. Write Path / Hot Tier / Main Serve Lack Enterprise Hardening (High)
- **Files:** `internal/store/store.go:581` (Append: `s.mu.Lock()` + full validation + commit with fsync), `502` (writeApplyNotify), `90` (RWMutex); `cmd/centauri/main.go:465` (normal serve NewWithOptions, no wrapper); no limits on LLM paths (`internal/ceql/ai.go:82` 300s default timeout synchronous).
- **Description:** Single-writer serialization unchanged. Hot tail + full index (even with `-lazy` payloads) still RAM-heavy for the active set. No per-request timeouts, result limits, or admission on main path. Named DBs (`?db=`) share the same process/locks (see prior production-suggestions issues).
- **Suggestion:** (Longer-term) Introduce query admission on hot path; consider time-bounded context propagation for LLM calls; add resource accounting.
- **Enterprise impact:** Matches prior gaps doc exactly: "Long cold scans or LLM calls can starve other work" still true for non-lazy; "Fundamental Single-Writer Architecture" unaddressed.

### 6. Secrets, Logging, Compliance, HA (Medium)
- **Secrets:** `internal/ceql/ai.go:53,77` and `enrich.go:73,341` (`os.Getenv(env)` for `auth_env`). No Vault/KMS. `Dockerfile` + main just pass `$CENTAURI_TOKEN`.
- **Logging:** `cmd/centauri/main.go` (hundreds of `log.Fatal`/`log.Printf`), `internal/store/*`, `api/*` use `log` + `fmt` exclusively. No levels, no correlation IDs, no structured output. `/metrics` added but no traces.
- **HA/Multi-tenancy:** `main.go:345` (wantsLock), single process. `follow`/`sync` manual. No leader election. Named envs have isolation gaps (prior review: api.go:84 lookupACL default store, byName without Lock).
- **Files:** `internal/store/store.go` (no encryption on hot tail/manifest/lazy.ckpt), `docs/enterprise-readiness.md:39` (notes "use volume/disk encryption").
- **Suggestion:** Structured logging (slog or equivalent, keeping zero-dep spirit); document token/env best practices; update prior ACL/locking issues from production-suggestions.md.
- **Enterprise impact:** Regulated/compliance (HIPAA etc.) and cloud-native ops friction. Hot tier at-rest still partial.

### 7. Other / Documentation & Deployment Drift
- **Files:** `docs/deploy.md:11-12,34` (still says "plain HTTP", "never expose", recommends only proxy; no mention of `-tls-cert` or `-lazy-index`); `cmd/centauri/main.go:458` (normal serve banner omits metrics/health).
- Lazy read path implementation is solid (zone pruning in scan.go:83-126, LRU in archive_reader.go:78-108, checkpoint Merkle validation in lazyindex.go:129-154).
- No write-side equivalent of LazyRoutes.

---

## Review of enterprise-readiness.md

**Accuracy per row (focus on recently added ✓ rows):**

### "What it does" section
| Row | Claimed | Actual | Assessment |
|-----|---------|--------|------------|
| Native TLS / HTTPS | ✓ — `-tls-cert`/`-tls-key` on `serve` and `serve -lazy-index` | Yes (listenMaybeTLS + flags on multiple paths) | Accurate. Works for normal + lazy. |
| Auth on the read path | ✓ — `serve -lazy-index` data routes (`/v1/*`) require a read token; dashboard/health/metrics stay open | Yes for lazy (lazyAuth + tests in lazy_test.go:115-126). Normal path had prior auth via ReadToken. | Mostly accurate but phrasing implies this is new/only for lazy; UI dashboard is intentionally open on both (no fact data). Good intent note. |
| Prometheus metrics + health probes | ✓ — `/metrics` (text), `/livez`, `/readyz` | Implemented, but **only** inside `LazyRoutes` (lazy.go:203,206,217). No equivalent in normal `api.Routes()`. | **Inaccurate / incomplete.** The matrix presents as general capability. Does not qualify "when running `serve -lazy-index`". Overstatement. Live dashboard (lazy.html:151) repeats the same. |
| (Other prior ✓ rows) | e.g. Scales beyond RAM, crypto-erasure, etc. | Confirmed via lazyindex + archive paths. | Accurate. |

### "What it does not do (yet)" section
- Rate limiting / quotas / admission control | partial — describes `-lazy-index` only + "write path ... not yet limited" | Matches code exactly. | Accurate and honest.
- Structured logging / OpenTelemetry traces | ✗ — line-oriented | Accurate.
- Others (SQL, multi-writer, object-store, retention, auto-failover) | ✗/partial | Remain true. | No overstatements.

**Suggestions for improvement:**
- Qualify observability rows: "Prometheus metrics + health probes (lazy-index read path)" or add a general row noting `/v1/stats` + "standard probes only on `-lazy-index`".
- Add a row or note for "Admission control" under "does" as partial/lazy-only to match the "does not" entry.
- Update "Auth on the read path" to mention that normal `serve` also supports `-token` / `-read-token` on data routes (the lazy addition closed the bypass gap).
- Add missing item: "Health/readiness probes and Prometheus metrics on the primary (non-lazy) serve path".
- Add missing: "Write-path admission control / rate limiting / query timeouts".
- Clarify TLS: "Basic native TLS (no mTLS/client certs)".
- Sync the live matrix in `internal/api/lazy.html` (it duplicates the claims).
- Cross-link to `deploy.md` caveats (currently drifts).

**Completeness:** Covers the deliberate differentiators well. Gaps in "does not" are honest. One over-claim on observability. No invented ✓.

---

## Addressed vs Remaining Gaps

Comparison to `docs/enterprise-deployment-gaps.md` (2026-06-18, pre-updates):

| Category from Prior Gaps | Prior Status | Now Addressed? | Details / Citations |
|--------------------------|--------------|----------------|---------------------|
| No Built-in TLS / HTTPS | Gap (plain HTTP only) | **Yes (basic)** | `-tls-cert`/`-tls-key` + listenMaybeTLS (main.go:85-86,1241). Available on serve + lazy. Deploy.md not updated. |
| Lazy-Index / Archive Path Bypasses Auth | Explicit gap (main.go old ListenAndServe no wrapper) | **Yes** | `lazyAuth` (lazy.go:256-272) + `LazyRoutes(li, lazyTok)` (main.go:338). Tests cover (lazy_test.go:115). Dashboard/health open intentionally. |
| Missing Prometheus /metrics | Gap | **Yes (lazy only)** | /metrics in lazy.go:217-239 (cache, segments, resident keys). |
| No K8s liveness/readiness probes | Gap | **Yes (lazy only)** | /livez /readyz in lazy.go:203-212 (manifest check on readyz). |
| No query timeouts / admission | Gap (table row) | **Yes (lazy only)** | WithLimits (limits.go) + flags (main.go:87-88,335-338). 429/503 behavior. |
| Rate limiting / quotas | Absent | Partial | Only lazy global; write + per-tenant open. |
| Structured logging / OTel | Missing | **Unchanged** | Still log./fmt everywhere. |
| Write path single-writer / no limits | Fundamental gap | **Unchanged** | store.go:581 mu.Lock; no wrapper on normal serve. |
| Secrets / KMS | Env vars only | **Unchanged** | os.Getenv in ceql/ai.go + enrich.go. |
| Hot tier at-rest encryption | Partial (only sealed) | **Unchanged** | Tail + lazy.ckpt + manifest unprotected. |
| HA / auto failover / leader | Manual (follow/sync) | **Unchanged** | No election. |
| Multi-tenancy isolation | Limited (named DBs) | **Unchanged** | Shared process; prior ACL/lock issues persist (api.go:84 etc.). |
| Normal serve health/metrics | N/A (not called out) | New gap surfaced | Only lazy has them. |
| Docs drift (deploy.md) | Proxy recommended | New observation | Still claims no TLS support in places. |

**Summary of progress:** ~4 of the top "quick wins" from prior recommendations implemented, but **confined to the `-lazy-index` codepath**. Core write/hot/main path + cross-cutting concerns (logs, secrets, HA, write limits) remain. The readiness doc is now more current but needs qualification for accuracy.

---

## Recommendations

**Immediate (for ops/enterprise users of tablespaces):**
1. Use `serve -lazy-index` when you need the new probes/metrics/limits/TLS+auth story (read-only cold path).
2. For mixed workloads, run separate lazy read replicas + normal writer (via `follow`).
3. Add reverse proxy or terminate TLS at LB even with native support (defense in depth).
4. Scrape `/metrics` and wire `/readyz` only for lazy instances; adapt `/v1/stats` + `centauri doctor` for normal.

**Short-term code/docs:**
- Port minimal healthz + metrics to normal `Server` (or factor common handlers).
- Apply or document limits behavior for non-lazy.
- Update `docs/deploy.md` to document `-tls-cert`/`-tls-key` and `-lazy-index` usage.
- Align `/v1/version` auth behavior.
- Address lingering multi-db ACL/lock issues noted in prior reviews (production-suggestions.md).

**Longer-term (ROADMAP alignment):**
- Write-side admission + per-tenant controls.
- Structured logging + correlation.
- External secrets + hot-tier encryption hooks.
- Better multi-process / sharded isolation or clear HA orchestration guidance.
- Full capability parity (or explicit "lazy-only" labels) between server modes.

**Testing/Validation notes:** `go test ./...` (internal/api + store) passes for lazy routes/limits. No end-to-end K8s or TLS integration tests observed. Run with real certs + token + concurrency load for production validation.

This review is objective and cites exact locations. The updates are a positive step; remaining gaps are accurately reflected in the "does not" section of readiness.md once the observability claims are qualified.

*Generated via direct code + doc inspection. No fixes applied.*