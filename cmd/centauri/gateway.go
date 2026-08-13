package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aniljacobv-lab/centauri/internal/api"
	"github.com/aniljacobv-lab/centauri/internal/gateway"
	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
)

// runGatewayCmd implements `centauri gateway`: the API gateway with a perfect
// memory. Routes/keys are bi-temporal facts in the control-plane log (AS OF
// auditable, live-reloaded via the store watch) and every proxied request is
// appended to the same tamper-evident log. It owns its flag set because its
// defaults differ from serve (gateway.log, :8080).
func runGatewayCmd(args []string) {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	data := fs.String("data", "gateway.log", "control-plane log (routes, keys, audit)")
	addr := fs.String("addr", ":8080", "proxy listen address")
	admin := fs.String("admin", "", "also serve the standard dashboard/API on this address over the same store (e.g. :7771)")
	adminInline := fs.Bool("admin-inline", false, "serve the dashboard/API on the PROXY port for anything no route matches — single-port deploys (Render etc.); routes always win")
	token := fs.String("token", os.Getenv("CENTAURI_TOKEN"), "bearer token required on the admin API (empty = open)")
	readToken := fs.String("read-token", os.Getenv("CENTAURI_READ_TOKEN"), "second admin token granting read-only access")
	tlsCert := fs.String("tls-cert", "", "PEM certificate for HTTPS on the proxy listener (with -tls-key)")
	tlsKey := fs.String("tls-key", "", "PEM private key for HTTPS on the proxy listener (with -tls-cert)")
	newKey := fs.String("newkey", "", "mint an API key with this friendly name: append its gwkey fact, print the key ONCE, exit")
	_ = fs.Parse(args)
	*data = ensureDataPath(*data)

	st, err := store.OpenOptions(*data, store.Options{Lock: true})
	if err != nil {
		log.Fatalf("gateway: open store: %v (is a gateway already running on this log?)", err)
	}

	// -newkey mints a key offline: 32 random bytes, hex-encoded; only its
	// sha256 enters the log. Runs under the same single-writer lock, so stop
	// the gateway first (or mint before starting it).
	if *newKey != "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			st.Close()
			log.Fatalf("gateway: entropy: %v", err)
		}
		secret := hex.EncodeToString(raw[:])
		sum := sha256.Sum256([]byte(secret))
		ev := &model.Event{
			Subject: "gwkey:" + hex.EncodeToString(sum[:]), Facet: "config", Type: model.Observed,
			Value:      map[string]any{"name": *newKey},
			Provenance: model.HumanEntry, Confidence: 1, SourceSystem: "GATEWAY",
		}
		if err := st.Append(time.Now().UnixMicro(), []*model.Event{ev}, nil); err != nil {
			st.Close()
			log.Fatalf("gateway: mint key: %v", err)
		}
		st.Close()
		fmt.Printf("minted API key %q — shown ONCE, only its sha256 is stored:\n\n  %s\n\n", *newKey, secret)
		fmt.Println("callers send it as  Authorization: Bearer <key>  or  X-Api-Key: <key>")
		fmt.Println("scope/limit it:     CORRECT gwkey:" + hex.EncodeToString(sum[:]) + " FACET config SET name='" + *newKey + "', rate_per_min=600")
		fmt.Println("revoke it:          RETIRE gwkey:" + hex.EncodeToString(sum[:]) + " FACET config")
		return
	}

	gw := gateway.New(st)
	gw.Start()

	var adminSrv *api.Server
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		gw.Stop() // drains queued audit facts into the log
		if adminSrv != nil {
			adminSrv.Close()
		}
		_ = st.Close()
		os.Exit(0)
	}()

	scheme := "http"
	if *tlsCert != "" && *tlsKey != "" {
		scheme = "https"
	}
	fmt.Print(banner)
	fmt.Println(api.BuildLine())
	fmt.Printf("gateway: the API gateway with a perfect memory\n")
	fmt.Printf("control plane: %s   (routes/keys are bi-temporal facts; audit is tamper-evident)\n", *data)
	routes := gw.RouteList()
	fmt.Printf("routes loaded: %d\n", len(routes))
	for _, rt := range routes {
		target := rt.Upstream
		if rt.LLM {
			target = fmt.Sprintf("llm x%d provider(s)", len(rt.Providers))
		}
		fmt.Printf("  %-24s -> %s  (auth=%s rate=%d/min)\n", rt.Prefix, target, rt.Auth, rt.RatePerMin)
	}
	if *admin != "" {
		adminSrv = api.NewWithOptions(st, api.Options{Token: *token, ReadToken: *readToken, DataPath: *data})
		logger := api.SetupLogger("text", "info")
		go func() {
			log.Fatal(listenMaybeTLS(*admin, "", "", api.WithLogging(adminSrv.Routes(), logger)))
		}()
		fmt.Printf("admin dashboard: http://localhost%s%s\n", *admin, authNote(*token))
	} else if !*adminInline {
		fmt.Println("admin dashboard: off — add -admin :7771 (or -admin-inline) to manage routes in the browser")
	}
	if *adminInline {
		if adminSrv == nil {
			adminSrv = api.NewWithOptions(st, api.Options{Token: *token, ReadToken: *readToken, DataPath: *data})
		}
		logger := api.SetupLogger("text", "info")
		gw.Fallback = api.WithLogging(adminSrv.Routes(), logger)
		fmt.Printf("admin dashboard: inline on the proxy port — any path no route matches%s\n", authNote(*token))
	}
	fmt.Println("\nadd a route (CeQL, in the admin console/shell — applies live, no restart):")
	fmt.Printf("  %s\n", gateway.ExampleRoutePut)
	fmt.Println("mint a key (stop the gateway first — single-writer lock):")
	fmt.Printf("  centauri gateway -data %s -newkey ci\n", *data)
	fmt.Printf("\nproxy listening on %s://localhost%s   metrics: /metrics   health: /livez /readyz\n", scheme, *addr)
	fmt.Println("note: llm routes are non-streaming for now — \"stream\":true is rewritten to false.")
	log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, gw))
}
