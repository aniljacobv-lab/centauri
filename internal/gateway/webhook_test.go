package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookAllowAndPayload(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	var raw []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "auth": "key", "authz_webhook": hook.URL})
	secret := mintKey(t, st, g, "hook-key-secret", "ci", nil, 0)

	rec := doGet(t, g, "/api/x", map[string]string{
		"Authorization": "Bearer " + secret,
		"User-Agent":    "test-agent",
		"Content-Type":  "application/json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook allow: code=%d body=%q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("webhook payload not JSON: %q", raw)
	}
	if got["method"] != "GET" || got["path"] != "/api/x" || got["key"] != "ci" || got["ip"] != "10.1.2.3" {
		t.Fatalf("webhook payload wrong: %#v", got)
	}
	hdr, _ := got["headers"].(map[string]any)
	if hdr["user-agent"] != "test-agent" || hdr["content-type"] != "application/json" {
		t.Fatalf("safe header subset missing: %#v", hdr)
	}
	// The caller's credential must never reach the webhook.
	if strings.Contains(string(raw), secret) || strings.Contains(strings.ToLower(string(raw)), "authorization") {
		t.Fatalf("credential leaked to webhook: %q", raw)
	}
}

func TestWebhookDeny(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(hook.Close)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "authz_webhook": hook.URL})

	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "authorization webhook") {
		t.Fatalf("webhook deny: code=%d body=%q", rec.Code, rec.Body.String())
	}
	g.Flush()
	cur := st.Current("gw:api", "gateway")
	if len(cur) != 1 || cur[0].Value["webhook_denied"] != true {
		t.Fatalf("denial audit missing webhook_denied: %#v", cur)
	}
}

func TestWebhookTimeoutFailClosed(t *testing.T) {
	old := webhookTimeout
	webhookTimeout = 150 * time.Millisecond
	t.Cleanup(func() { webhookTimeout = old })

	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hang past the deadline; return promptly once the caller gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hook.Close)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "authz_webhook": hook.URL})

	rec := doGet(t, g, "/api/x", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("webhook timeout must fail closed: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestWebhookFailOpenFlag(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	// Nothing listens here — the callout fails instantly.
	dead := "http://127.0.0.1:1/authz"

	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "authz_webhook": dead})
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("unreachable webhook, fail-closed default: code=%d", rec.Code)
	}

	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "authz_webhook": dead, "webhook_fail_open": true})
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("webhook_fail_open: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// A route whose webhook URL cannot work is rejected whole (fail closed),
// like a bad CIDR or glob.
func TestWebhookBadURLRejectsRoute(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{
		"prefix": "/api/", "upstream": up.srv.URL, "authz_webhook": "not a url"})
	if rec := doGet(t, g, "/api/x", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("route with broken webhook URL took traffic: code=%d", rec.Code)
	}
}
