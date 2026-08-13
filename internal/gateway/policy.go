package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/proc"
)

// Request policies — "plugins, the Centauri way". A route with
// policy:"<name>" runs the stored CePL procedure proc:<name> after auth,
// rate limiting, and the built-in middleware, immediately before proxying.
// The policy is itself a fact: versioned, AS OF auditable, hot-reloaded.
// It is parsed ONCE per config load (loadSnapshot) and only executed per
// request — see BenchmarkPolicyOverhead for the measured per-call cost.
// Fail-closed everywhere: a route whose policy is missing or unparsable is
// rejected at load time, and a runtime error or FAIL denies with 403.

// policyArgs is the fixed argument set a policy procedure may declare (any
// subset): method, path, key (resolved caller name, "" when anonymous), ip.
func policyArgs(r *http.Request, keyName string) map[string]any {
	return map[string]any{
		"method": r.Method,
		"path":   r.URL.Path,
		"key":    keyName,
		"ip":     clientIP(r),
	}
}

// applyPolicy runs the route's pre-parsed policy procedure. It returns false
// when the request was finished here (denied). Every denial is audited with
// the policy's name — even on audit:"none" routes: a policy rejection is a
// security event, not traffic volume.
func (g *Gateway) applyPolicy(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time) bool {
	res, err := proc.Run(g.st, rt.policyProc, policyArgs(r, keyName), time.Now().UnixMicro())
	if err != nil {
		// FAIL or a runtime error: fail closed, with the procedure's message.
		return g.denyByPolicy(w, r, rt, keyName, start, http.StatusForbidden, err.Error())
	}
	allow, status, msg, addHeaders := policyDecision(res.Return)
	if !allow {
		return g.denyByPolicy(w, r, rt, keyName, start, status, msg)
	}
	for k, v := range addHeaders {
		r.Header.Set(k, v) // reaches the upstream request
	}
	return true
}

// policyDecision interprets a policy's RETURN value:
//
//	true / 'allow'                      -> proceed
//	false / 'deny' / 'deny:<msg>'       -> 403 (with msg)
//	{"allow":bool, "status":N, "message":s, "add_headers":{..}}
//	                                    -> honored (status default 403;
//	                                       add_headers only when allowed)
//
// Anything else is not recognizably an allow — deny, fail closed.
func policyDecision(ret any) (allow bool, status int, msg string, addHeaders map[string]string) {
	status, msg = http.StatusForbidden, "denied by route policy"
	switch v := ret.(type) {
	case bool:
		allow = v
	case string:
		switch {
		case v == "allow":
			allow = true
		case v == "deny":
		case strings.HasPrefix(v, "deny:"):
			msg = strings.TrimSpace(strings.TrimPrefix(v, "deny:"))
		default:
			msg = fmt.Sprintf("policy returned %q — not a decision", v)
		}
	case map[string]any:
		allow = boolField(v, "allow")
		if m := strField(v, "message"); m != "" {
			msg = m
		}
		if s := intField(v, "status"); s >= 400 && s <= 599 {
			status = s
		}
		if allow {
			addHeaders = strMap(v, "add_headers")
		}
	default:
		msg = "policy returned no decision"
	}
	return allow, status, msg, addHeaders
}

func (g *Gateway) denyByPolicy(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time, status int, msg string) bool {
	ms := sinceMS(start)
	g.met.observe(rt.Name, status, ms)
	jsonError(w, status, msg)
	g.auditDenial(rt, r.Method, r.URL.Path, status, ms, keyName, map[string]any{"policy": rt.Policy})
	return false
}

// --- webhook authorization callout ---

// webhookTimeout bounds the authorization callout. A var so tests can shrink it.
var webhookTimeout = 2 * time.Second

// webhookClient is shared; the per-call deadline comes from the request context.
var webhookClient = &http.Client{}

// applyWebhook POSTs {method, path, key, ip, headers} to the route's
// authz_webhook and allows the request only on a 2xx answer. Anything else —
// a non-2xx status, a timeout, a refused connection — is a 403, unless the
// route opted into webhook_fail_open. The forwarded headers are a fixed safe
// subset (content-type, user-agent, x-request-id); Authorization and cookies
// are never sent to the webhook. Denials are audited with webhook_denied.
func (g *Gateway) applyWebhook(w http.ResponseWriter, r *http.Request, rt *Route, keyName string, start time.Time) bool {
	hdr := map[string]string{}
	for _, k := range []string{"Content-Type", "User-Agent", "X-Request-Id"} {
		if v := r.Header.Get(k); v != "" {
			hdr[strings.ToLower(k)] = v
		}
	}
	body, _ := json.Marshal(map[string]any{
		"method":  r.Method,
		"path":    r.URL.Path,
		"key":     keyName,
		"ip":      clientIP(r),
		"headers": hdr,
	})
	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	allowed := false
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.AuthzWebhook, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		if resp, werr := webhookClient.Do(req); werr == nil {
			resp.Body.Close()
			allowed = resp.StatusCode/100 == 2
		}
	}
	if allowed || rt.WebhookFailOpen {
		return true
	}
	ms := sinceMS(start)
	g.met.observe(rt.Name, http.StatusForbidden, ms)
	jsonError(w, http.StatusForbidden, "denied by authorization webhook")
	g.auditDenial(rt, r.Method, r.URL.Path, http.StatusForbidden, ms, keyName, map[string]any{"webhook_denied": true})
	return false
}
