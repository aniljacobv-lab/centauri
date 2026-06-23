# Centauri — Enterprise Feature Implementation Guide

**Date:** 2026-06-23  
**Purpose:** Concrete, prioritized recommendations for implementing the remaining enterprise deployment gaps identified in the latest code review (post-tablespaces + recent enterprise updates).

This guide focuses on features marked as gaps or partial in:
- `docs/enterprise-review-rerun.md`
- `docs/enterprise-readiness.md` ("does not" / partial rows)
- `docs/enterprise-deployment-gaps.md` (prior review)

**Core constraints** (from CLAUDE.md and project design):
- Zero third-party Go dependencies (stdlib only).
- Preserve append-only invariants, write-then-apply, hash chain, replay determinism.
- Lazy-index path is deliberately read-only and separate.
- Documentation honesty.

## Prioritization

1. **High (Quick wins for ops/K8s/observability)**
2. **Medium (Security hardening, consistency)**
3. **Lower / Longer-term (HA, advanced compliance)**

---

## 1. Observability: Health Probes + Prometheus Metrics on Primary Serve Path

**Gap:** `/livez`, `/readyz`, and `/metrics` exist only for `serve -lazy-index`. Normal `serve` (the primary path) lacks standard K8s/Prometheus targets.

**Current state (lazy example):**
- `internal/api/lazy.go:203-212` (`/livez`, manifest-based `/readyz`)
- `internal/api/lazy.go:217-239` (Prometheus text format with `centauri_lazy_*` series)
- Exposed only via `LazyRoutes`

### Implementation Recommendations

**Step 1: Add lightweight handlers to the main `Server`**

In `internal/api/api.go`, inside `Routes()` or a new `commonHandlers()`:

```go
// Add after the existing mux setup
mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte("ok"))
})

mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
    st := s.dbOr(w, r)  // or just check default
    if st == nil {
        http.Error(w, "not ready", http.StatusServiceUnavailable)
        return
    }
    // Lightweight check: can we read the manifest / chain head?
    if _, err := st.ChainHead(); err != nil {
        http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
        return
    }
    w.Write([]byte("ready"))
})

mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
    // Reuse or adapt from lazy
    st := s.dbOr(w, r)
    if st == nil { return }

    // Basic store stats (already exposed via /v1/stats, but Prometheus format)
    stats := st.Stats()
    w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
    fmt.Fprintf(w, "# HELP centauri_resident_keys Current facts\n")
    fmt.Fprintf(w, "# TYPE centauri_resident_keys gauge\n")
    fmt.Fprintf(w, "centauri_resident_keys %d\n", stats["open"])

    // Add more: events, subjects, pending, etc.
    // For lazy-specific metrics, keep them in lazy path only
})
```

**Step 2: Expose version consistently**
- Move `/v1/version` behind or outside auth consistently (currently unauthed in main, gated in lazy for `/v1/`).

**Step 3: Wire in main.go**
- For normal serve: wrap the final handler if desired, but keep simple.
- Update banners (main.go:319, 454 etc.) to always mention metrics/health when relevant.
- For lazy, keep the existing richer `centauri_lazy_*` metrics.

**Step 4: Update tests & docs**
- Extend `lazy_test.go` style tests to main server.
- Update `deploy.md` and `enterprise-readiness.md` (qualify the observability row).

**Considerations:**
- Keep probes extremely lightweight (no full DB scan).
- For multi-DB (`?db=`), decide whether `/livez` is global or per-db.
- Prometheus labels can include `db` or `mode`.

**Effort estimate:** 1-2 days (mostly wiring + tests).

---

## 2. Admission Control & Rate Limiting on Main/Write Path

**Gap:** `WithLimits` + `-max-concurrency` / `-query-timeout` only apply to lazy read path. Write path and normal queries (including LLM `ENRICH`/`ASK`) are unbounded.

**Current lazy impl:** `internal/api/limits.go` (semaphore + `http.TimeoutHandler`).

### Implementation Recommendations

**Option A — Simple per-path wrappers (quick)**
Extend `WithLimits` or create `WithQueryLimits`.

In `api.go` Routes():

```go
heavyRead := s.handleQuery  // or wrap specific ones
mux.HandleFunc("GET /v1/query", WithLimits(heavyRead, maxConc, timeout))
mux.HandleFunc("POST /v1/query", WithLimits(heavyRead, maxConc, timeout))

// For writes (lighter or separate queue)
mux.HandleFunc("POST /v1/append", s.write(WithLimits(s.handleAppend, writeConc, writeTimeout)))
```

**Option B — Global middleware + config**
- Parse the flags in `NewWithOptions` or pass from main.
- Add a `limits` middleware in `auth` chain or root.
- For writes: use a small buffered channel or semaphore around `Append` calls (respect single-writer lock).

**For LLM-heavy paths** (`ceql/ai.go`, `enrich.go`):
- Propagate `context` with timeout from the HTTP request into `Infer` / `chatLLM`.
- Currently timeouts are per-model in config; make HTTP-level timeout take precedence.

**Write-path specific:**
- Since writes are single-writer, admission can be a simple in-flight counter outside the lock.
- Reject early with 429 before validation/lock acquisition.

**Flags & defaults:**
- Extend existing `-max-concurrency` / `-query-timeout` to normal serve (document "applies to heavy queries on lazy by default; enable for main with care").
- Add `-write-concurrency` if needed.

**Considerations:**
- Do not hold the store mutex while waiting on semaphore (deadlock risk).
- For SSE/watch: be careful with long-lived connections.
- Test with the existing `concurrency_test.go` pattern.

**Effort:** 2-3 days.

---

## 3. Improve TLS Experience & Documentation

**Current:** Basic `http.ListenAndServeTLS` via `listenMaybeTLS`.

### Recommendations

1. **Cert reloading (SIGHUP or periodic)**
   - Use `crypto/tls` `GetCertificate` callback with a hot-reloadable `tls.Config`.
   - Example sketch in a new `tls.go` or inside `listenMaybeTLS`.

2. **Better validation & logging**
   - On startup with TLS flags: load and validate certs early.
   - Log "TLS enabled" + fingerprint (without leaking private material).

3. **mTLS (client cert auth) as optional flag**
   - `-tls-client-ca` for CA bundle.
   - Integrate with existing token auth as fallback or additional layer.

4. **Docs & banners**
   - Update `deploy.md` to show `-tls-cert` examples alongside proxy recipes.
   - Update banners in `main.go` when TLS is active.
   - Mention that reverse proxy is still recommended for rate-limiting/WAF/TLS termination in production.

5. **ALPN / HTTP/2**
   - Stdlib `ListenAndServeTLS` supports h2 when using `http2` package (import `_ "golang.org/x/net/http2"` if needed, but keep zero-dep — document how to enable).

**Effort:** 1-3 days depending on reload + mTLS depth.

---

## 4. Consistent Auth & Reduce Path Asymmetry

**Current:**
- Lazy: only `/v1/*` gated; dashboard/health open.
- Normal: data mux under `auth()`, several UI endpoints intentionally open.

### Recommendations

- Extract `lazyAuth` logic into reusable middleware in `api/auth.go`.
- Decide on `/v1/version` behavior (make consistent — probably unauthenticated in both).
- Document the "open endpoints carry no user data" policy clearly in both code comments and docs.
- For named DBs (`?db=`): ensure ACL lookup (already improved) and future limits respect the target store.

**For future RBAC expansion:**
- Keep ACL facts as the mechanism.
- Add helper in `Server` for role expansion later.

**Effort:** Low (mostly refactoring + docs).

---

## 5. Structured Logging & Observability Foundations

**Current:** Heavy use of `log.Fatal`, `log.Printf`, `fmt`.

### Recommendations (zero-dep friendly)

**Option: Use `log/slog` (Go 1.21+ stdlib)**
- Default to text handler.
- Add `-log-format=json` flag.
- Emit structured attributes for key events: `append`, `query`, `enrich`, `llm_call`, `segment_read`, etc.

**Minimal wrapper** if staying pre-slog:

```go
type logger struct {
    *log.Logger
}

func (l *logger) Info(msg string, kv ...any) { ... } // simple key=value or JSON
```

**Correlation:**
- Generate request ID in middleware (or use `X-Request-ID`).
- Pass via context to store/ceql calls.
- Include in logs and error responses.

**For LLM calls:**
- Log duration, model kind, success/failure (with truncation) at INFO level.

**Metrics expansion:**
- Once `/metrics` is on main path, add counters for appends, queries, LLM calls, errors.

**Effort:** 2-4 days (slog is easiest).

---

## 6. Secrets & Hot-Tier Protection

**Secrets:**
- Document best practices: use Kubernetes secrets / Docker secrets mounted as env, or init containers.
- Future hook: allow `auth_env` to be a file path (read at call time).
- For model registration: consider a `PUT model:...` with optional `secret_ref`.

**Hot tier at-rest:**
- Recommend (and document) running on encrypted volumes (LUKS, BitLocker, cloud disk encryption).
- For sealed segments, the existing per-key AES is the differentiator.
- Future: optional transparent encryption for tail using a key from env/file (similar to crypto-erasure).

**Effort for hooks:** Low for docs + file-path support.

---

## 7. Write-Path & Multi-Tenancy Hardening

**Write admission:**
- Add a simple `sync.Mutex` + counter around append paths (outside store lock).
- Or integrate with `WithLimits` for query-like writes.

**Named DBs / tenants:**
- Ensure `byName` always passes `Lock: true` (already mostly done).
- Fix any remaining ACL lookup issues for `?db=` (see `api.go:272`).
- Consider per-DB resource accounting (future).

**LLM paths:**
- Add request-scoped timeout context to `InferRequest`.
- Track in-flight LLM calls.

---

## 8. Documentation & Deployment Polish (Quick)

**Must-do:**
- Update `docs/deploy.md` with TLS examples, `-lazy-index` usage, new health/metrics endpoints, and admission flags.
- Update banners in `main.go` for normal serve to mention available endpoints.
- Add a "Production Checklist" section in `enterprise-readiness.md` or new file (tokens, TLS, volume encryption, monitoring, backup of chain head + manifests).
- Document that `-lazy-index` is the recommended path when you need the new enterprise controls for large read-only workloads.

**Tests:**
- Add integration tests that exercise TLS + token + limits + metrics together (use `httptest` + temp certs).

---

## Cross-Cutting Advice

- **Keep it simple & zero-dep:** Prefer stdlib (`net/http` middleware, `context`, `log/slog`, `crypto/tls`).
- **Preserve separation:** Lazy remains read-only high-scale path. Don't force full feature parity if it bloats the hot path.
- **Backward compat:** New flags default to "off" or "no limit". Existing behavior unchanged unless flags are set.
- **Testing:** Extend existing concurrency tests. Add load tests for limits/timeout behavior.
- **Staged rollout:** Implement on lazy first (already happening), then port to main.
- **Metrics naming:** Follow the `centauri_*` convention started in lazy.

## Suggested Next Steps (in order)

1. Port healthz + basic Prometheus metrics to normal `Server` (highest ops impact).
2. Apply `WithLimits` (or lighter version) to heavy query paths in normal serve.
3. Update all documentation (deploy.md + banners + readiness matrix qualifiers).
4. Add basic cert reload + improve TLS startup validation.
5. Introduce simple structured logging (slog) + request IDs.
6. Add write-path admission counter.
7. Address any remaining multi-DB ACL/locking nits from prior reviews.

Implementing the top 3-4 items would close most of the "enterprise ops" friction for both normal and tablespace-scale deployments.

---

*This guide is derived from code inspection of the current state. Update it as implementations land.*