// Centauri, by Proxima360 — a bi-temporal, causal, self-enriching,
// AI-first event database. v0.2: single binary; append-only log storage
// with checkpoints; HTTP/JSON + MCP (agent) interfaces; log-shipping
// read replicas.
//
//	centauri seed   -data centauri.log -skus 25 -stores 40
//	centauri serve  -data centauri.log -addr :7771 [-token SECRET]
//	centauri mcp    -data centauri.log
//	centauri follow -data replica.log -primary http://host:7771 [-addr :7772] [-token SECRET]
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/proxima360/centauri/internal/api"
	"github.com/proxima360/centauri/internal/catalog"
	"github.com/proxima360/centauri/internal/mcp"
	"github.com/proxima360/centauri/internal/store"
	"github.com/proxima360/centauri/internal/synth"
)

// apiSrv lets the shutdown path close named environments opened at runtime.
var apiSrv *api.Server

const banner = `
  ___  ____  _  _  ____  __    _  _  ____  __
 / __)(  __)( \( )(_  _)/ _\  / )( \(  _ \(  )
( (__  ) _) /    /  )( /    \ ) \/ ( )   / )(
 \___)(____)\_)__) (__)\_/\_/ \____/(__\_)(__)
        Centauri v0.2 — by Proxima360
   every fact knows its time, cause, and source
`

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	data := fs.String("data", "centauri.log", "path to the append-only log")
	addr := fs.String("addr", ":7771", "listen address (serve; optional for follow)")
	token := fs.String("token", os.Getenv("CENTAURI_TOKEN"), "bearer token required on the HTTP API (empty = open)")
	skus := fs.Int("skus", 25, "synthetic SKU count (seed)")
	stores := fs.Int("stores", 40, "synthetic store count (seed)")
	changes := fs.Int("changes", 5, "price changes per subject (seed)")
	seedVal := fs.Int64("rand", 42, "rng seed (seed)")
	readToken := fs.String("read-token", os.Getenv("CENTAURI_READ_TOKEN"), "second token granting read-only access (serve)")
	to := fs.String("to", "", "destination file (backup)")
	primary := fs.String("primary", "", "primary base URL to replicate from (follow)")
	interval := fs.Duration("interval", 2*time.Second, "poll interval (follow)")
	_ = fs.Parse(os.Args[2:])

	// backup copies the committed log (safe while the server runs — the
	// log only ever appends, and we stop at the last complete record),
	// then proves the copy with its chain head.
	if cmd == "backup" {
		if *to == "" {
			log.Fatal("backup: -to <destination file> is required")
		}
		head, size, records, err := store.VerifyChain(*data)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		src, err := os.Open(*data)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		dst, err := os.Create(*to)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		if _, err := io.CopyN(dst, src, size); err != nil {
			log.Fatalf("backup: copy: %v", err)
		}
		src.Close()
		if err := dst.Sync(); err != nil {
			log.Fatalf("backup: sync: %v", err)
		}
		dst.Close()
		copyHead, _, _, err := store.VerifyChain(*to)
		if err != nil || copyHead != head {
			log.Fatalf("backup: copy verification FAILED (%v) — do not trust %s", err, *to)
		}
		fmt.Printf("backup OK: %s -> %s\nrecords: %d  bytes: %d\nchain head (both files): %s\n",
			*data, *to, records, size, head)
		return
	}

	// verify never opens the store for writing — it only reads bytes.
	if cmd == "verify" {
		head, size, records, err := store.VerifyChain(*data)
		if err != nil {
			log.Fatalf("verify: %v", err)
		}
		fmt.Printf("log:        %s\nrecords:    %d\nbytes:      %d\nchain head: %s\n",
			*data, records, size, head)
		fmt.Println("\nRecord this chain head somewhere external. If a future verify")
		fmt.Println("reproduces it, the history is byte-for-byte intact (tamper-evident).")
		return
	}

	// seed is a redoable bulk load: skip per-commit fsync for speed.
	// Everything else syncs every commit so acknowledged writes survive
	// a crash.
	st, err := store.OpenOptions(*data, store.Options{NoSync: cmd == "seed"})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Graceful shutdown: Close syncs and writes the checkpoint that makes
	// the next Open fast. log.Fatal skips deferred calls, so long-running
	// commands rely on this signal path.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if apiSrv != nil {
			apiSrv.Close() // named environments
		}
		_ = st.Close()
		os.Exit(0)
	}()

	// serve and follow both default -addr to :7771; nudge followers off
	// the primary's port so a single-host demo works out of the box.
	if cmd == "follow" && *addr == ":7771" {
		*addr = ":7772"
	}

	switch cmd {
	case "seed":
		stats, err := synth.Seed(st, *skus, *stores, *changes, rand.New(rand.NewSource(*seedVal)))
		if err != nil {
			log.Fatalf("seed: %v", err)
		}
		fmt.Print(banner)
		fmt.Printf("seeded: %+v\n", stats)
	case "serve":
		fmt.Print(banner)
		// The command catalog lives in its own database next to the data
		// file; the dashboard reads autocomplete from it — suggestions are
		// data, not code, and cost no AI tokens.
		if n, err := catalog.SeedIfEmpty(*data, time.Now().UnixMicro()); err != nil {
			log.Printf("catalog: %v (autocomplete falls back to built-ins)", err)
		} else {
			fmt.Printf("command catalog: %d commands in ceql-catalog\n", n)
		}
		fmt.Printf("data: %s\nlistening on %s\n", *data, *addr)
		fmt.Println(`try:  curl 'localhost:7771/v1/stats'
      curl 'localhost:7771/v1/context?subject=item:100001/store:4001'
      curl 'localhost:7771/v1/asof?subject=item:100001/store:4001&at=2026-03-15'
      curl 'localhost:7771/v1/pending?facet=pdt&older_than_days=21'
      curl 'localhost:7771/v1/disagreements?field=price_cents'
      curl -N 'localhost:7771/v1/watch?facet=pdt'`)
		srv := api.NewWithOptions(st, api.Options{Token: *token, ReadToken: *readToken, DataPath: *data})
		apiSrv = srv
		log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
	case "mcp":
		// stdio is the protocol channel; keep it clean of banners.
		if err := mcp.New(st, os.Stdin, os.Stdout).Run(); err != nil {
			log.Fatalf("mcp: %v", err)
		}
	case "follow":
		if *primary == "" {
			log.Fatal("follow: -primary is required")
		}
		fmt.Print(banner)
		fmt.Printf("replicating %s -> %s every %s\n", *primary, *data, *interval)
		if *addr != "" {
			srv := api.NewWithOptions(st, api.Options{Token: *token, ReadOnly: true})
			go func() {
				fmt.Printf("read-only API on %s\n", *addr)
				log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
			}()
		}
		follow(st, *primary, *token, *interval)
	default:
		usage()
	}
}

// follow polls the primary's log endpoint and ingests new bytes forever.
func follow(st *store.Store, primary, token string, interval time.Duration) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		from := st.LogSize()
		url := fmt.Sprintf("%s/v1/log?from=%d", primary, from)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Fatalf("follow: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("follow: primary unreachable: %v (retrying)", err)
			time.Sleep(interval)
			continue
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("follow: read: %v (retrying)", err)
			time.Sleep(interval)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			log.Printf("follow: primary returned %d: %s (retrying)", resp.StatusCode, string(b))
			time.Sleep(interval)
			continue
		}
		if len(b) > 0 {
			if err := st.IngestRaw(b); err != nil {
				// Do not crash on a possibly-transient problem; the next
				// poll refetches from our committed offset.
				log.Printf("follow: ingest at offset %d: %v (retrying)", from, err)
				time.Sleep(interval)
				continue
			}
			log.Printf("follow: ingested %d bytes (log now %d)", len(b), st.LogSize())
			continue // immediately ask for more; we may be catching up
		}
		time.Sleep(interval)
	}
}

func usage() {
	fmt.Print(banner)
	fmt.Println(`usage: centauri <command> [flags]

  seed    populate with synthetic price-change events
  serve   HTTP/JSON API (writes + queries + SSE watch + log shipping)
  mcp     Model Context Protocol server on stdio (for AI agents)
  follow  replicate a primary's log into a read-only follower
  verify  recompute the tamper-evidence chain over a log file
  backup  copy a database to -to <file> and verify the copy's chain`)
	os.Exit(1)
}
