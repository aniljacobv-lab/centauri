package api

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
)

// The Centauri Console — the product-suite home page (gateway, keys, usage,
// health, learning links), one self-contained HTML file embedded in the
// binary and served at /console. Like the dashboard it ships no data of its
// own; every call it makes carries the bearer token the user entered (shared
// with the dashboard via the same localStorage key).
//
//go:embed console.html
var consoleHTML []byte

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(consoleHTML)
}

// handleGatewayKeyMint mints a gateway API key server-side:
//
//	POST /v1/gateway/keys {"name":"ci-bot","rate_per_min":60,"routes":["/api/"]}
//
// The server generates a 32-byte random secret and stores ONLY its SHA-256 as
// a gwkey:<hash> config fact — the contract in internal/gateway/config.go —
// then returns the secret exactly once. The secret never enters the fact log
// or the server's own logs. Admin-gated like every other write. It works both
// in-process with a running gateway (-admin/-admin-inline share this store,
// so the gateway's watch loop picks the key up on commit) and on a plain
// database (the fact is inert until a gateway serves it).
func (s *Server) handleGatewayKeyMint(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	var body struct {
		Name       string   `json:"name"`
		RatePerMin int      `json:"rate_per_min"`
		Routes     []string `json:"routes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		httpErr(w, 400, "name is required — audit facts record it as the caller")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		httpErr(w, 500, "cannot generate key: "+err.Error())
		return
	}
	secret := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	val := map[string]any{"name": name}
	if body.RatePerMin > 0 {
		val["rate_per_min"] = body.RatePerMin
	}
	routes := make([]any, 0, len(body.Routes))
	for _, p := range body.Routes {
		if p = strings.TrimSpace(p); p != "" {
			routes = append(routes, p)
		}
	}
	if len(routes) > 0 {
		val["routes"] = routes
	}
	e := &model.Event{
		Subject: "gwkey:" + hash, Facet: "config", Type: model.Observed,
		Value:      val,
		Provenance: model.HumanEntry, Confidence: 1.0, SourceSystem: "GATEWAY-CONSOLE",
	}
	if err := st.Append(time.Now().UnixMicro(), []*model.Event{e}, nil); err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"key":     secret,
		"subject": e.Subject,
		"note":    "shown once — only its hash is stored",
	})
}
