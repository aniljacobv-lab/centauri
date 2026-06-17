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
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/proxima360/centauri/internal/api"
	"github.com/proxima360/centauri/internal/assistant"
	"github.com/proxima360/centauri/internal/catalog"
	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/mcp"
	"github.com/proxima360/centauri/internal/model"
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
	to := fs.String("to", "", "destination file (backup, export)")
	query := fs.String("q", "FACTS OF *", "CeQL query selecting what to export (export)")
	format := fs.String("format", "csv", "export format: csv | jsonl (export)")
	primary := fs.String("primary", "", "primary base URL to replicate from (follow)")
	interval := fs.Duration("interval", 2*time.Second, "poll interval (follow)")
	lazy := fs.Bool("lazy", false, "keep event payloads on disk instead of RAM (serve/desktop; lets data exceed RAM)")
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

	// merge reconciles diverged copies (e.g. the same data edited on two
	// devices via a synced folder) into one new log. Offline & non-destructive.
	if cmd == "merge" {
		if *to == "" {
			log.Fatal("merge: -to <output.log> is required")
		}
		inputs := fs.Args()
		if len(inputs) == 0 {
			log.Fatal("merge: give one or more input logs, e.g. centauri merge -to merged.log a.log b.log")
		}
		n, err := store.MergeLogs(*to, inputs...)
		if err != nil {
			log.Fatalf("merge: %v", err)
		}
		fmt.Printf("merged %d unique records from %d log(s) into %s — it replays cleanly.\n", n, len(inputs), *to)
		fmt.Printf("verify it: centauri verify -data %s\n", *to)
		return
	}

	// seed is a redoable bulk load: skip per-commit fsync for speed.
	// Everything else syncs every commit so acknowledged writes survive
	// a crash.
	st, err := store.OpenOptions(*data, store.Options{NoSync: cmd == "seed",
		Lock: cmd == "serve" || cmd == "desktop", LazyPayloads: *lazy})
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
	case "desktop":
		// The double-click experience: data lives in the user's profile
		// (no stray files in random folders), the browser opens itself,
		// and the window explains what's happening. Installers point
		// their shortcuts here.
		if !flagWasSet(fs, "data") {
			dir, derr := os.UserConfigDir()
			if derr == nil {
				dir = filepath.Join(dir, "Centauri")
				if err := os.MkdirAll(dir, 0o755); err == nil {
					*data = filepath.Join(dir, "centauri.log")
					// reopen the store at the profile location
					st.Close()
					st, err = store.OpenOptions(*data, store.Options{Lock: true, LazyPayloads: *lazy})
					if err != nil {
						log.Fatalf("open store: %v", err)
					}
				}
			}
		}
		fmt.Print(banner)
		if n, err := catalog.SeedIfEmpty(*data, time.Now().UnixMicro()); err == nil {
			fmt.Printf("command catalog: %d commands\n", n)
		}
		if n, err := assistant.SeedIfEmpty(st, time.Now().UnixMicro()); err == nil {
			fmt.Printf("assistant: %d knowledge facts (ASK '…')\n", n)
		}
		fmt.Printf("your data:  %s\ndashboard:  http://localhost%s  (opening in your browser…)\n", *data, *addr)
		fmt.Printf("studio:     http://localhost%s/studio  (the AI-first IDE)\n", *addr)
		fmt.Println("\nKeep this window open while you use Centauri. Close it (or Ctrl+C) to stop.")
		go func() {
			time.Sleep(1200 * time.Millisecond)
			openBrowser("http://localhost" + *addr)
		}()
		srv := api.NewWithOptions(st, api.Options{Token: *token, DataPath: *data})
		apiSrv = srv
		log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
	case "export":
		if *to == "" {
			log.Fatal("export: -to <file> is required (e.g. -to facts.csv)")
		}
		if err := runExport(st, *query, *format, *to); err != nil {
			log.Fatalf("export: %v", err)
		}
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
		if n, err := assistant.SeedIfEmpty(st, time.Now().UnixMicro()); err == nil {
			fmt.Printf("assistant: %d knowledge facts (ASK '…')\n", n)
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

// flagWasSet reports whether the user explicitly passed a flag.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// openBrowser opens a URL with the platform's default browser.
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}

// runExport runs a CeQL read and writes the events as CSV or JSONL —
// the bridge to warehouses, Excel, and pandas.
func runExport(st *store.Store, query, format, to string) error {
	now := time.Now().UnixMicro()
	q, err := ceql.Parse(query, now)
	if err != nil {
		return err
	}
	res, err := ceql.Execute(st, q, now)
	if err != nil {
		return err
	}
	events, ok := res["events"].([]*model.Event)
	if !ok {
		return fmt.Errorf("-q must be a query returning events (FACTS/HISTORY without projection); got kind %v", res["kind"])
	}
	f, err := os.Create(to)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	switch format {
	case "jsonl":
		enc := json.NewEncoder(w)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	case "csv":
		// columns: fixed metadata + the union of value fields, sorted.
		fieldSet := map[string]bool{}
		for _, e := range events {
			for k := range e.Value {
				fieldSet[k] = true
			}
		}
		var fields []string
		for k := range fieldSet {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		cw := csv.NewWriter(w)
		header := append([]string{"subject", "facet", "type", "effective_time",
			"recorded_time", "confidence", "provenance", "event_id"}, fields...)
		if err := cw.Write(header); err != nil {
			return err
		}
		for _, e := range events {
			row := []string{e.Subject, e.Facet, string(e.Type),
				strconv.FormatInt(e.EffectiveTime, 10), strconv.FormatInt(e.RecordedTime, 10),
				strconv.FormatFloat(e.Confidence, 'f', -1, 64), string(e.Provenance), e.EventID}
			for _, k := range fields {
				v, ok := e.Value[k]
				if !ok {
					row = append(row, "")
					continue
				}
				switch t := v.(type) {
				case string:
					row = append(row, t)
				case float64:
					row = append(row, strconv.FormatFloat(t, 'f', -1, 64))
				default:
					b, _ := json.Marshal(v)
					row = append(row, string(b))
				}
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown format %q (csv | jsonl)", format)
	}
	fmt.Printf("exported %d events -> %s (%s)\n", len(events), to, format)
	return nil
}

func usage() {
	fmt.Print(banner)
	fmt.Println(`usage: centauri <command> [flags]

Commands
  desktop  one-click start: data in your profile, dashboard opens in your browser
  serve    HTTP/JSON API — writes, queries, SSE watch, log shipping
  mcp      Model Context Protocol server on stdio (connect AI agents)
  seed     populate with synthetic price-change demo data
  follow   replicate a primary's log into a read-only follower
  verify   recompute the tamper-evidence hash chain over a log file
  backup   copy a database to -to <file> and verify the copy's chain
  merge    reconcile diverged copies: merge -to merged.log a.log b.log
  export   write a CeQL result: export -q "FACTS OF *" -format csv -to out.csv

Common flags
  -data <path>    log file to use            (default centauri.log)
  -addr <:port>   listen address (serve)     (default :7771)
  -token <tok>    HTTP API bearer token      (or set $CENTAURI_TOKEN)
  -lazy           keep payloads on disk so data can exceed RAM (serve/desktop)

Examples
  centauri desktop                 # try it now — opens the dashboard
  centauri serve -lazy -token s3cr # API server, payloads on disk
  centauri merge -to all.log a.log b.log

Learn CeQL: run 'centauri desktop' and open the textbook at /ceql.`)
	os.Exit(1)
}
