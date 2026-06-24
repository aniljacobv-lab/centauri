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
	"strings"
	"syscall"
	"time"

	"github.com/proxima360/centauri/internal/api"
	"github.com/proxima360/centauri/internal/assistant"
	"github.com/proxima360/centauri/internal/catalog"
	"github.com/proxima360/centauri/internal/ceql"
	"github.com/proxima360/centauri/internal/demo"
	"github.com/proxima360/centauri/internal/mcp"
	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/retention"
	"github.com/proxima360/centauri/internal/shard"
	"github.com/proxima360/centauri/internal/store"
	"github.com/proxima360/centauri/internal/synth"
)

// apiSrv lets the shutdown path close named environments opened at runtime.
var apiSrv *api.Server

// managedOllama is a local Ollama process that THIS Centauri started (because
// it wasn't already running). We stop it on exit; an Ollama the user was
// already running is never touched.
var managedOllama *os.Process

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
	format := fs.String("format", "table", "output format: table | csv | jsonl | json | yaml | plain (export)")
	primary := fs.String("primary", "", "primary base URL to replicate from (follow)")
	interval := fs.Duration("interval", 2*time.Second, "poll interval (follow)")
	lazy := fs.Bool("lazy", false, "keep event payloads on disk instead of RAM (serve/desktop; lets data exceed RAM)")
	install := fs.Bool("install", false, "with 'setup vision': install missing prerequisites via your OS package manager")
	manageOllama := fs.Bool("ollama", true, "desktop: auto-start a local Ollama if one isn't running, and stop it on exit (only the one we start)")
	segMax := fs.Int("seg-max", 100000, "records per segment (archive)")
	lazyIndex := fs.Bool("lazy-index", false, "serve: open an archive with the disk-backed index — RAM scales with live subjects, not total events (read-only: current/history/asof)")
	tlsCert := fs.String("tls-cert", "", "serve/desktop: PEM certificate file for native HTTPS (with -tls-key)")
	tlsKey := fs.String("tls-key", "", "serve/desktop: PEM private-key file for native HTTPS (with -tls-cert)")
	maxConc := fs.Int("max-concurrency", 0, "serve/desktop/lazy-index: cap in-flight non-streaming requests (0=unlimited); excess gets HTTP 429")
	queryTimeout := fs.Int("query-timeout", 0, "serve/desktop/lazy-index: per-request timeout in seconds (0=none); slow requests get HTTP 503 (streaming endpoints exempt)")
	maxConcPerDB := fs.Int("max-concurrency-per-db", 0, "serve/desktop: per-tenant (per ?db=) in-flight request cap (0=none); excess gets HTTP 429")
	logFormat := fs.String("log-format", "text", "serve/desktop: structured request log format: text | json")
	logLevel := fs.String("log-level", "info", "serve/desktop: log level: debug | info | warn | error")
	retPattern := fs.String("pattern", "", "retention: subject glob to retire, e.g. 'log:*'")
	olderThan := fs.Int("older-than", 0, "retention: retire subjects whose newest fact is older than N days")
	applyRet := fs.Bool("apply", false, "retention: actually RETIRE (default is a dry-run plan)")
	groupCommit := fs.Bool("group-commit", false, "serve/desktop: coalesce concurrent appends into one fsync (higher write throughput under load; experimental)")
	shards := fs.Int("shards", 0, "serve: sharded write-scaling mode with N shard logs under -data (a directory); writes to different subjects run in parallel")
	checkpointEvery := fs.Int("checkpoint-every", 0, "serve/desktop: write the recovery checkpoint every N seconds (0=only on clean shutdown); bounds crash-recovery replay")
	autoSealMB := fs.Int("auto-seal-mb", 0, "serve on an archive dir: auto-seal the tail into a segment once it exceeds N MB (0=manual); bounds the hot log")
	fastVerify := fs.Bool("fast", false, "verify (archive dir): parallel per-segment Merkle scrub across cores; omit for the full sequential chain check")
	compactGroup := fs.Int("group", 8, "compact: merge this many consecutive segments into one")
	_ = fs.Parse(os.Args[2:])
	logger := api.SetupLogger(*logFormat, *logLevel)
	archiveMode := isArchiveDir(*data) // a dir with manifest.json = a sealed-segment archive
	if !archiveMode {
		*data = ensureDataPath(*data) // a folder path (e.g. OneDrive dir) just works
	}

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
		// An archive directory verifies its segments; -fast scrubs per-segment
		// Merkle roots in parallel (across cores), the default adds the full
		// sequential cross-segment chain check.
		if isArchiveDir(*data) {
			if *fastVerify {
				records, err := store.VerifyArchiveParallel(*data)
				if err != nil {
					log.Fatalf("verify: %v", err)
				}
				fmt.Printf("archive:    %s\nrecords:    %d\nverified:   per-segment Merkle roots, in parallel ✓\n", *data, records)
				fmt.Println("(per-segment integrity only — run 'verify' without -fast for the cross-segment chain.)")
				return
			}
			head, records, err := store.VerifyArchive(*data)
			if err != nil {
				log.Fatalf("verify: %v", err)
			}
			fmt.Printf("archive:    %s\nrecords:    %d\nchain head: %s\nverified:   per-segment Merkle + continuous hash chain ✓\n", *data, records, head)
			return
		}
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

	// doctor is a read-only health check: it never opens the store for
	// writing, so it is safe to run against a live database.
	if cmd == "doctor" {
		runDoctor(*data, *to)
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

	// demo seeds (or clears) a dedicated, disposable "demo.log" sibling of the
	// data file: a curated multi-domain dataset for learning Centauri. "clear"
	// drops the whole demo database file — it never touches the real log, so it
	// is consistent with "nothing is ever erased" (that rule is about facts
	// within a live log, not disposable example databases).
	if cmd == "demo" {
		demoPath := filepath.Join(filepath.Dir(*data), "demo.log")
		switch fs.Arg(0) {
		case "seed":
			dst, err := store.OpenOptions(demoPath, store.Options{NoSync: true, Lock: true})
			if err != nil {
				log.Fatalf("demo: %v", err)
			}
			if demo.Seeded(dst) {
				dst.Close()
				fmt.Printf("demo database already seeded: %s\n(use 'centauri demo clear' to reset it)\n", demoPath)
				return
			}
			res, err := demo.Seed(dst, time.Now().UnixMicro())
			dst.Close()
			if err != nil {
				log.Fatalf("demo seed: %v", err)
			}
			fmt.Print(banner)
			fmt.Printf("seeded demo database: %s\n%+v\n", demoPath, res.Stats)
			fmt.Println("\nserve, open the Studio, switch the database selector to \"demo\", then try:")
			for _, sg := range res.Suggestions {
				fmt.Printf("  [%-12s] %s\n", sg.Domain, sg.Query)
			}
		case "clear":
			os.Remove(demoPath)
			os.Remove(demoPath + ".checkpoint")
			fmt.Printf("cleared demo database: %s\n", demoPath)
		default:
			log.Fatal("usage: centauri demo seed   |   centauri demo clear")
		}
		return
	}

	// setup gets a local AI ready so an average user doesn't have to install
	// Ollama + a PDF renderer by hand. Centauri can't bundle them (zero-dep,
	// and the models are multi-GB), but it orchestrates the install/pull.
	if cmd == "setup" {
		switch fs.Arg(0) {
		case "vision":
			runSetupVision(*install)
		default:
			log.Fatal("usage: centauri setup vision [-install]")
		}
		return
	}

	// archive seals a log into compressed, Merkle-rooted, zone-mapped segments
	// (a "tablespace" layout) + a manifest, then verifies it. Non-destructive:
	// the source log is only read. The archive's chain head equals the live
	// store's, proving the compressed copy is byte-faithful and tamper-evident.
	if cmd == "archive" {
		if *to == "" {
			log.Fatal("archive: -to <dir> is required")
		}
		man, err := store.WriteArchive(*data, *to, *segMax)
		if err != nil {
			log.Fatalf("archive: %v", err)
		}
		head, recs, err := store.VerifyArchive(*to)
		if err != nil {
			log.Fatalf("archive verify FAILED: %v", err)
		}
		var comp int64
		for _, s := range man.Segments {
			comp += s.Bytes
		}
		fmt.Print(banner)
		fmt.Printf("archived:  %s -> %s\n", *data, *to)
		fmt.Printf("segments:  %d   records: %d\n", len(man.Segments), recs)
		fmt.Printf("chain head: %s\n", head)
		if fi, e := os.Stat(*data); e == nil && fi.Size() > 0 {
			fmt.Printf("size:      %d -> %d bytes  (%.0f%% of original — compressed + tamper-evident)\n",
				fi.Size(), comp, 100*float64(comp)/float64(fi.Size()))
		}
		fmt.Println("verified:  Merkle roots + continuous hash chain across all segments ✓")
		return
	}

	// seal rolls a served/idle archive's appendable tail into a new compressed,
	// tamper-evident segment (crash-safe atomic manifest switch). Run it on an
	// archive directory; it takes the single-writer lock, so don't run it while
	// 'serve' holds that archive.
	if cmd == "seal" {
		if !isArchiveDir(*data) {
			log.Fatalf("seal: %s is not an archive directory — run 'centauri archive -data <log> -to %s' first", *data, *data)
		}
		a, err := store.OpenArchive(*data, store.Options{Lock: true})
		if err != nil {
			log.Fatalf("seal: %v", err)
		}
		if err := a.Seal(); err != nil {
			a.Close()
			log.Fatalf("seal: %v", err)
		}
		gc, _ := store.GCArchive(*data) // sweep any crash-orphaned files (lock held)
		a.Close()
		head, recs, verr := store.VerifyArchive(*data)
		if verr != nil {
			log.Fatalf("seal: post-seal verify FAILED: %v", verr)
		}
		fmt.Printf("sealed the tail into a new segment.\nrecords: %d   chain head: %s\n", recs, head)
		if len(gc) > 0 {
			fmt.Printf("cleaned %d orphaned file(s)\n", len(gc))
		}
		fmt.Println("verified ✓")
		return
	}

	// tablespace-demo runs the whole tablespaces lifecycle end-to-end on a fresh,
	// comprehensive + bulk dataset: seed -> archive (compress + Merkle) -> lazy
	// open -> retrieve every way (current/lookup/history/asof/search/trace/verify)
	// -> insert (append a fact + a correction) -> re-archive -> show the updated
	// state. It writes everything under a directory (default ./tablespace-demo).
	if cmd == "tablespace-demo" {
		dir := *data
		if dir == "" || dir == "centauri.log" {
			dir = "tablespace-demo"
		}
		segM := *segMax
		if !flagWasSet(fs, "seg-max") {
			segM = 2000 // smaller segments so the demo shows a multi-segment tablespace
		}
		runTablespaceDemo(dir, segM)
		return
	}

	// serve -shards N runs sharded write-scaling mode: subjects are partitioned
	// across N independent shard logs under -data (a directory) and written in
	// parallel. Routed reads + single-subject CeQL; wildcard/global queries use
	// the single-store serve path.
	if cmd == "serve" && *shards > 1 {
		dir := *data
		if dir == "" || dir == "centauri.log" {
			dir = "centauri-shards"
		}
		set, err := shard.Open(dir, *shards, store.Options{Lock: true, GroupCommit: *groupCommit, LazyPayloads: *lazy,
			CheckpointEvery: time.Duration(*checkpointEvery) * time.Second})
		if err != nil {
			log.Fatalf("open shards: %v", err)
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() { <-sig; _ = set.Close(); os.Exit(0) }()

		tok := *readToken
		if tok == "" {
			tok = *token
		}
		scheme := "http"
		if *tlsCert != "" && *tlsKey != "" {
			scheme = "https"
		}
		fmt.Print(banner)
		fmt.Println(api.BuildLine())
		fmt.Printf("sharded write-scaling: %d shards under %s%s\n", *shards, dir, authNote(tok))
		fmt.Printf("info/health: %s://localhost%s   (parallel writes · /v1/shards · /metrics)\n", scheme, *addr)
		fmt.Printf("listening on %s\n", *addr)
		h := api.WithLimits(api.ShardRoutes(set, tok), *maxConc, time.Duration(*queryTimeout)*time.Second)
		h = api.WithLogging(h, logger)
		log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, h))
	}

	// serve -lazy-index runs the disk-backed read path: open an archive WITHOUT
	// replaying it into RAM — only the current fact per (subject,facet) stays
	// resident, so memory scales with live subjects rather than total events. It
	// answers current/history/asof (history/asof stream pruned segments from
	// disk). Read-only by design; for writes, serve the archive normally.
	if cmd == "serve" && *lazyIndex {
		if !archiveMode {
			log.Fatalf("-lazy-index needs -data to be a sealed-segment archive directory (run 'centauri archive' first)")
		}
		li, err := store.OpenLazyIndex(*data)
		if err != nil {
			log.Fatalf("open lazy index: %v", err)
		}
		// Persist the pointer-checkpoint so the next start replays only the tail.
		if err := li.SaveCheckpoint(); err != nil {
			log.Printf("lazy checkpoint: %v (restart will rebuild from segments)", err)
		}
		fmt.Print(banner)
		fmt.Println(api.BuildLine())
		// A read token (if set) gates the data routes; the dashboard/health/metrics
		// stay open. Prefer -read-token here since the lazy path is read-only.
		lazyTok := *readToken
		if lazyTok == "" {
			lazyTok = *token
		}
		scheme := "http"
		if *tlsCert != "" && *tlsKey != "" {
			scheme = "https"
		}
		fmt.Printf("lazy disk-backed index: %d live keys resident (RAM scales with subjects, not events)\n", li.Keys())
		fmt.Printf("data:      %s   (read-only%s)\n", *data, authNote(lazyTok))
		fmt.Printf("dashboard: %s://localhost%s   (storage inspector · verify · query console · cache metrics)\n", scheme, *addr)
		fmt.Printf("listening on %s   metrics: /metrics   health: /livez /readyz\n", *addr)
		if *maxConc > 0 || *queryTimeout > 0 {
			fmt.Printf("limits:    max-concurrency=%d  query-timeout=%ds\n", *maxConc, *queryTimeout)
		}
		handler := api.WithLimits(api.LazyRoutes(li, lazyTok), *maxConc, time.Duration(*queryTimeout)*time.Second)
		handler = api.WithLogging(handler, logger) // outermost: log every request incl. 429/503
		log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, handler))
	}

	// compact merges consecutive sealed segments into fewer, larger ones (fewer
	// files, smaller manifest) — never erasing, chain preserved. Offline: it
	// takes the single-writer lock, so don't run it against a live server.
	if cmd == "compact" {
		if !isArchiveDir(*data) {
			log.Fatalf("compact: %s is not an archive directory (run 'centauri archive' first)", *data)
		}
		before, after, err := store.CompactArchive(*data, *compactGroup)
		if err != nil {
			log.Fatalf("compact: %v", err)
		}
		fmt.Print(banner)
		fmt.Printf("compacted: %s\nsegments:  %d -> %d (merged in groups of %d, in parallel)\n", *data, before, after, *compactGroup)
		fmt.Println("verified:  chain head unchanged — nothing erased, lines preserved in order ✓")
		return
	}

	// seed is a redoable bulk load: skip per-commit fsync for speed.
	// Everything else syncs every commit so acknowledged writes survive
	// a crash.
	wantsLock := cmd == "serve" || cmd == "desktop" || cmd == "shell" || cmd == "sync" || cmd == "retention"
	var st *store.Store
	var err error
	if archiveMode {
		// Run directly on a sealed-segment archive (compressed + tamper-verified).
		st, err = store.OpenArchive(*data, store.Options{Lock: wantsLock, LazyPayloads: *lazy,
			AutoSealBytes: int64(*autoSealMB) << 20, CheckpointEvery: time.Duration(*checkpointEvery) * time.Second})
	} else {
		st, err = store.OpenOptions(*data, store.Options{NoSync: cmd == "seed", Lock: wantsLock, LazyPayloads: *lazy,
			GroupCommit:     *groupCommit && (cmd == "serve" || cmd == "desktop"),
			CheckpointEvery: time.Duration(*checkpointEvery) * time.Second})
	}
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
		if managedOllama != nil {
			_ = managedOllama.Kill() // stop only the Ollama we started
		}
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
		if _, e := exec.LookPath("ollama"); e != nil {
			fmt.Println("\nvision (optional): Ollama not detected — run 'centauri setup vision -install' to let an AI read images & PDFs, all local. The dashboard shows a setup banner too.")
		} else if *manageOllama {
			managedOllama = ensureOllama() // start it if it isn't running; we'll stop it on exit
		}
		if note := syncedFolderNote(*data); note != "" {
			fmt.Println("\n" + note)
		}
		fmt.Println("\nKeep this window open while you use Centauri. Close it (or Ctrl+C) to stop.")
		go func() {
			time.Sleep(1200 * time.Millisecond)
			openBrowser("http://localhost" + *addr)
		}()
		srv := api.NewWithOptions(st, api.Options{Token: *token, DataPath: *data,
			MaxConcurrent: *maxConc, RequestTimeout: time.Duration(*queryTimeout) * time.Second,
			MaxConcurrentPerDB: *maxConcPerDB})
		apiSrv = srv
		log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, api.WithLogging(srv.Routes(), logger)))
	case "export":
		if err := runExport(st, *query, *format, *to); err != nil {
			log.Fatalf("export: %v", err)
		}
	case "shell":
		runShell(st)
	case "retention":
		rep, err := retention.Run(st, *retPattern, *olderThan, *applyRet, time.Now().UnixMicro())
		if err != nil {
			log.Fatalf("retention: %v", err)
		}
		fmt.Print(banner)
		verb := "would retire (dry run)"
		if rep.Applied {
			verb = "retired"
		}
		fmt.Printf("retention  pattern=%q  older-than=%dd\n", rep.Pattern, rep.OlderThanDays)
		fmt.Printf("scanned: %d   held: %d   due: %d   %s: %d\n",
			rep.Scanned, len(rep.Held), len(rep.Due), verb, len(rep.Retired))
		if len(rep.Held) > 0 {
			fmt.Printf("skipped under legal hold: %v\n", rep.Held)
		}
		if !rep.Applied && len(rep.Due) > 0 {
			fmt.Println("due (dry run — re-run with -apply to RETIRE; history is always kept):")
			for _, s := range rep.Due {
				fmt.Println("  " + s)
			}
		}
		for _, e := range rep.Errors {
			fmt.Println("  error: " + e)
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
		fmt.Println(api.BuildLine())
		if note := syncedFolderNote(*data); note != "" {
			fmt.Println(note)
		}
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
		fmt.Printf("data: %s\nlistening on %s   metrics: /metrics   health: /livez /readyz\n", *data, *addr)
		fmt.Println(`try:  curl 'localhost:7771/v1/stats'
      curl 'localhost:7771/v1/context?subject=item:100001/store:4001'
      curl 'localhost:7771/v1/asof?subject=item:100001/store:4001&at=2026-03-15'
      curl 'localhost:7771/v1/pending?facet=pdt&older_than_days=21'
      curl 'localhost:7771/v1/disagreements?field=price_cents'
      curl -N 'localhost:7771/v1/watch?facet=pdt'`)
		srv := api.NewWithOptions(st, api.Options{Token: *token, ReadToken: *readToken, DataPath: *data,
			MaxConcurrent: *maxConc, RequestTimeout: time.Duration(*queryTimeout) * time.Second,
			MaxConcurrentPerDB: *maxConcPerDB})
		apiSrv = srv
		log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, api.WithLogging(srv.Routes(), logger)))
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
				log.Fatal(listenMaybeTLS(*addr, *tlsCert, *tlsKey, api.WithLogging(srv.Routes(), logger)))
			}()
		}
		follow(st, *primary, *token, *interval)
	case "sync":
		if *primary == "" {
			log.Fatal("sync: -primary <peer base URL> is required")
		}
		fmt.Print(banner)
		fmt.Printf("bidirectional sync with %s every %s (run sync on the peer too)\n", *primary, *interval)
		syncPeer(st, *primary, *token, *interval)
	default:
		usage()
	}
}

// syncPeer continuously pulls a peer's new facts (CDC) and ingests the ones we
// don't already hold, tracking progress in a per-peer replication slot. Run it
// on both nodes pointing at each other for bidirectional, echo-safe sync.
func syncPeer(st *store.Store, peer, token string, interval time.Duration) {
	client := &http.Client{Timeout: 60 * time.Second}
	slot := "sync:" + peer
	for {
		from := st.SlotCursor(slot)
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/v1/changes?from=%d", peer, from), nil)
		if err != nil {
			log.Fatalf("sync: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("sync: peer unreachable: %v (retrying)", err)
			time.Sleep(interval)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Printf("sync: peer returned %d: %s (retrying)", resp.StatusCode, string(body))
			time.Sleep(interval)
			continue
		}
		var page struct {
			Events   []*model.Event `json:"events"`
			Cursor   int64          `json:"cursor"`
			CaughtUp bool           `json:"caught_up"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			log.Printf("sync: bad page: %v (retrying)", err)
			time.Sleep(interval)
			continue
		}
		n, err := st.IngestForeign(page.Events)
		if err != nil {
			log.Printf("sync: ingest at cursor %d: %v (retrying)", from, err)
			time.Sleep(interval)
			continue
		}
		if err := st.AdvanceSlot(time.Now().UnixMicro(), slot, page.Cursor); err != nil {
			log.Printf("sync: slot advance: %v", err)
		}
		if n > 0 {
			log.Printf("sync: pulled %d new fact(s) from %s (cursor %d)", n, peer, page.Cursor)
		}
		if page.CaughtUp {
			time.Sleep(interval)
		}
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

// runExport runs a CeQL read and writes the events in the chosen format.
// With -to empty or "-", it prints to stdout (handy for ad-hoc queries);
// otherwise it writes the file and prints a one-line summary.
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
	toStdout := to == "" || to == "-"
	var w *bufio.Writer
	var f *os.File
	if toStdout {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err = os.Create(to)
		if err != nil {
			return err
		}
		w = bufio.NewWriter(f)
	}
	if err := formatEvents(events, format, w); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if f != nil {
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Printf("exported %d events -> %s (%s)\n", len(events), to, format)
	}
	return nil
}

// formatEvents renders events to w in one of: csv, jsonl, json, table,
// yaml, plain. Pure (no I/O beyond w) so it is unit-testable.
func formatEvents(events []*model.Event, format string, w io.Writer) error {
	switch format {
	case "jsonl":
		enc := json.NewEncoder(w)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(events)
	case "csv":
		fields := valueFields(events)
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
				row = append(row, cellString(e.Value[k]))
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	case "table":
		fields := valueFields(events)
		cols := append([]string{"subject", "facet", "type"}, fields...)
		rows := make([][]string, 0, len(events))
		for _, e := range events {
			r := []string{e.Subject, e.Facet, string(e.Type)}
			for _, k := range fields {
				r = append(r, cellString(e.Value[k]))
			}
			rows = append(rows, r)
		}
		width := make([]int, len(cols))
		for i, c := range cols {
			width[i] = len(c)
		}
		for _, r := range rows {
			for i, c := range r {
				if len(c) > width[i] {
					width[i] = len(c)
				}
			}
		}
		writeRow := func(cells []string) {
			parts := make([]string, len(cells))
			for i, c := range cells {
				parts[i] = c + strings.Repeat(" ", width[i]-len(c))
			}
			fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
		}
		writeRow(cols)
		seps := make([]string, len(cols))
		for i := range seps {
			seps[i] = strings.Repeat("-", width[i])
		}
		fmt.Fprintln(w, strings.Join(seps, "  "))
		for _, r := range rows {
			writeRow(r)
		}
	case "yaml":
		for _, e := range events {
			fmt.Fprintf(w, "- subject: %s\n  facet: %s\n  type: %s\n  effective_time: %d\n  confidence: %s\n",
				e.Subject, e.Facet, string(e.Type), e.EffectiveTime, strconv.FormatFloat(e.Confidence, 'f', -1, 64))
			if len(e.Value) > 0 {
				fmt.Fprintln(w, "  value:")
				for _, k := range sortedKeys(e.Value) {
					fmt.Fprintf(w, "    %s: %s\n", k, cellString(e.Value[k]))
				}
			}
		}
	case "plain":
		for _, e := range events {
			var kvs []string
			for _, k := range sortedKeys(e.Value) {
				kvs = append(kvs, k+"="+cellString(e.Value[k]))
			}
			fmt.Fprintf(w, "%s [%s/%s] %s (conf %.2f)\n",
				e.Subject, e.Facet, string(e.Type), strings.Join(kvs, " "), e.Confidence)
		}
	default:
		return fmt.Errorf("unknown format %q (csv | jsonl | json | table | yaml | plain)", format)
	}
	return nil
}

// valueFields is the sorted union of value-field names across events.
func valueFields(events []*model.Event) []string {
	set := map[string]bool{}
	for _, e := range events {
		for k := range e.Value {
			set[k] = true
		}
	}
	var fs []string
	for k := range set {
		fs = append(fs, k)
	}
	sort.Strings(fs)
	return fs
}

func sortedKeys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// runDoctor prints a read-only health report: log presence, tamper-evidence
// chain integrity, checkpoint freshness, and writer-lock status. Exits
// non-zero if any check FAILs (e.g. a broken hash chain). Safe to run
// against a live server — it never opens the store for writing.
func runDoctor(data, reportTo string) {
	var b strings.Builder
	pass, warn, fail := 0, 0, 0
	line := func(status, msg string) {
		switch status {
		case "PASS":
			pass++
		case "WARN":
			warn++
		case "FAIL":
			fail++
		}
		fmt.Fprintf(&b, "[%s] %s\n", status, msg)
	}
	fmt.Fprintf(&b, "Centauri doctor — %s\n%s\n%s\n", data,
		time.Now().Format("2006-01-02 15:04:05"), strings.Repeat("-", 64))

	fi, statErr := os.Stat(data)
	if statErr != nil {
		line("FAIL", fmt.Sprintf("log file: %v", statErr))
	} else {
		line("PASS", fmt.Sprintf("log file present — %d bytes, modified %s",
			fi.Size(), fi.ModTime().Format("2006-01-02 15:04:05")))
		head, _, records, err := store.VerifyChain(data)
		switch {
		case err != nil:
			line("FAIL", fmt.Sprintf("tamper-evidence chain: %v", err))
		case records == 0:
			line("PASS", "empty log (new database) — chain trivially intact")
		default:
			line("PASS", fmt.Sprintf("tamper-evidence chain intact — %d records, head %s", records, shortHash(head)))
		}
		if cfi, err := os.Stat(data + ".checkpoint"); err == nil {
			if cfi.ModTime().Before(fi.ModTime()) {
				line("WARN", "checkpoint older than the log — it is rebuilt on next open (slower cold start)")
			} else {
				line("PASS", fmt.Sprintf("checkpoint present (%d bytes) — fast restart enabled", cfi.Size()))
			}
		} else {
			line("WARN", "no checkpoint — the next open replays the full log")
		}
		if _, err := os.Stat(data + ".lock"); err == nil {
			line("WARN", fmt.Sprintf("writer lock present — a server is running, or a crash left it stale (delete %s.lock if no server is up)", data))
		} else {
			line("PASS", "no writer lock — safe to start a server")
		}
	}
	fmt.Fprintf(&b, "%s\ndoctor: %d passed, %d warning(s), %d failed\n",
		strings.Repeat("-", 64), pass, warn, fail)
	fmt.Print(b.String())
	if reportTo != "" {
		if err := os.WriteFile(reportTo, []byte(b.String()), 0o644); err != nil {
			log.Printf("doctor: could not write report to %s: %v", reportTo, err)
		} else {
			fmt.Printf("report written to %s\n", reportTo)
		}
	}
	if fail > 0 {
		os.Exit(1)
	}
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

// runShell is a psql-style REPL over CeQL: type a query, see a table; meta
// commands start with a backslash. It runs against the local store directly
// (no HTTP), holding the writer lock so it can't race a running server.
func runShell(st *store.Store) {
	fmt.Print(banner)
	fmt.Println("Centauri shell — CeQL REPL.  \\h help · \\q quit · \\d subjects · \\timing · \\x expanded")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	timing, expanded := false, false
	format := "table"
	out := os.Stdout
	fmt.Print("ceql> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Print("ceql> ")
			continue
		}
		if strings.HasPrefix(line, "\\") {
			f := strings.Fields(line)
			switch f[0] {
			case "\\q", "\\quit":
				return
			case "\\h", "\\help", "\\?":
				printShellHelp()
			case "\\d", "\\subjects":
				line = "SUBJECTS"
			case "\\schemas":
				line = "SCHEMAS"
			case "\\stats":
				line = "STATS"
			case "\\slots":
				for _, sl := range st.Slots() {
					fmt.Fprintf(out, "%s\tcursor %d\n", sl.Name, sl.Cursor)
				}
				fmt.Print("ceql> ")
				continue
			case "\\timing":
				timing = !timing
				fmt.Printf("timing is %v\n", timing)
				fmt.Print("ceql> ")
				continue
			case "\\x":
				expanded = !expanded
				fmt.Printf("expanded display is %v\n", expanded)
				fmt.Print("ceql> ")
				continue
			case "\\format":
				if len(f) > 1 {
					format = f[1]
				}
				fmt.Printf("format is %s\n", format)
				fmt.Print("ceql> ")
				continue
			default:
				fmt.Println("unknown command; \\h for help")
				fmt.Print("ceql> ")
				continue
			}
			// Meta-commands that didn't rewrite `line` into a CeQL statement
			// (e.g. \h) must not fall through to the executor.
			if strings.HasPrefix(line, "\\") {
				fmt.Print("ceql> ")
				continue
			}
		}
		t0 := time.Now()
		q, err := ceql.Parse(line, time.Now().UnixMicro())
		if err != nil {
			fmt.Fprintln(out, "error:", err)
			fmt.Print("ceql> ")
			continue
		}
		res, err := ceql.Execute(st, q, time.Now().UnixMicro())
		if err != nil {
			fmt.Fprintln(out, "error:", err)
			fmt.Print("ceql> ")
			continue
		}
		printResult(out, res, format, expanded)
		if timing {
			fmt.Fprintf(out, "(%.1f ms)\n", float64(time.Since(t0).Microseconds())/1000.0)
		}
		fmt.Print("ceql> ")
	}
}

func printShellHelp() {
	fmt.Println(`CeQL: type any statement (FACTS OF item:*, PUT …, SEARCH '…', MATCH …, EXPLAIN ANALYZE …).
Meta:
  \d, \subjects   list subjects        \schemas   list schemas
  \stats          store counters       \slots     CDC replication slots
  \timing         toggle query timing  \x         toggle expanded (one field per line)
  \format <fmt>   table|csv|json|yaml|plain (for event results)
  \h              this help            \q         quit`)
}

// printResult renders a CeQL result for the shell. Event/row results get a
// table (or the chosen format); everything else prints as indented JSON.
func printResult(w io.Writer, res map[string]any, format string, expanded bool) {
	switch res["kind"] {
	case "events":
		evs, _ := res["events"].([]*model.Event)
		f := format
		if expanded {
			f = "yaml"
		}
		_ = formatEvents(evs, f, w)
		fmt.Fprintf(w, "(%d row(s))\n", len(evs))
	case "rows":
		cols, _ := res["columns"].([]string)
		rows, _ := res["rows"].([][]any)
		printRowsTable(w, cols, rows)
		fmt.Fprintf(w, "(%d row(s))\n", len(rows))
	case "subjects":
		subs, _ := res["subjects"].([]string)
		for _, s := range subs {
			fmt.Fprintln(w, s)
		}
		fmt.Fprintf(w, "(%d subject(s))\n", len(subs))
	default:
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(w, string(b))
	}
}

func printRowsTable(w io.Writer, cols []string, rows [][]any) {
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len(c)
	}
	cells := make([][]string, len(rows))
	for r, row := range rows {
		cells[r] = make([]string, len(cols))
		for i := range cols {
			var v any
			if i < len(row) {
				v = row[i]
			}
			cells[r][i] = cellString(v)
			if len(cells[r][i]) > width[i] {
				width[i] = len(cells[r][i])
			}
		}
	}
	line := func(vals []string) {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v + strings.Repeat(" ", width[i]-len(v))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	line(cols)
	seps := make([]string, len(cols))
	for i := range seps {
		seps[i] = strings.Repeat("-", width[i])
	}
	fmt.Fprintln(w, strings.Join(seps, "  "))
	for _, r := range cells {
		line(r)
	}
}

func usage() {
	fmt.Print(banner)
	fmt.Println(`usage: centauri <command> [flags]

Commands
  desktop  one-click start: data in your profile, dashboard opens in your browser
  serve    HTTP/JSON API — writes, queries, SSE watch, log shipping
  mcp      Model Context Protocol server on stdio (connect AI agents)
  shell    interactive CeQL REPL (psql-style; \h for meta commands)
  seed     populate with synthetic price-change demo data
  demo     seed|clear a curated multi-domain example database (demo.log)
           (tip: -data can be a folder, even a synced one like OneDrive)
  setup    setup vision [-install] — ready a local AI (Ollama + PDF renderer)
  follow   replicate a primary's log into a read-only follower
  sync     bidirectional, echo-safe sync with a peer (-primary <url>); run on both
  verify   recompute the tamper-evidence hash chain over a log file
  doctor   read-only health check (chain, checkpoint, lock); safe on a live DB
  backup   copy a database to -to <file> and verify the copy's chain
  archive  seal a log into compressed, tamper-evident segments: -to <dir>
  seal     roll an archive's tail into a new segment: -data <archive-dir>
  merge    reconcile diverged copies: merge -to merged.log a.log b.log
  export   run a CeQL query and print/write results in a chosen format

Common flags
  -data <path>    log file to use            (default centauri.log)
  -addr <:port>   listen address (serve)     (default :7771)
  -token <tok>    HTTP API bearer token      (or set $CENTAURI_TOKEN)
  -lazy           keep payloads on disk so data can exceed RAM (serve/desktop)
  -ollama         desktop: auto-start a local Ollama & stop it on exit (default on)
  -format <fmt>   table | csv | jsonl | json | yaml | plain (export)
  -to <file>      output file ('-' or omitted = stdout)  (export/backup/merge)

Examples
  centauri desktop                 # try it now — opens the dashboard
  centauri tablespace-demo         # seed→archive→retrieve→insert, end to end
  centauri doctor                  # is my database healthy?
  centauri export -q "FACTS OF item:*" -format table   # query to the terminal
  centauri serve -lazy -token s3cr # API server, payloads on disk

CDC: tail new facts over HTTP — GET /v1/changes?from=<cursor> returns events
plus a cursor to resume from. Learn CeQL at /ceql (run 'centauri desktop').`)
	os.Exit(1)
}

// isArchiveDir reports whether p is a directory holding a sealed-segment archive
// (a manifest.json) — in which case the engine opens it via OpenArchive.
func isArchiveDir(p string) bool {
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return false
	}
	_, err := os.Stat(filepath.Join(p, "manifest.json"))
	return err == nil
}

// runTablespaceDemo walks the full tablespaces lifecycle on a fresh dataset so a
// user can see — in one command — how comprehensive, bulk data is stored
// (compressed + tamper-evident), retrieved every way the lazy path supports, and
// inserted (with the new facts flowing straight back into the archive).
func runTablespaceDemo(dir string, segMax int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("tablespace-demo: %v", err)
	}
	logPath := filepath.Join(dir, "demo.log")
	arch := filepath.Join(dir, "archive")
	_ = os.Remove(logPath)
	_ = os.RemoveAll(arch)

	fmt.Print(banner)
	fmt.Println("Tablespaces demo — seed → archive → retrieve → insert → re-archive")
	fmt.Println(strings.Repeat("-", 66))

	// 1) SEED: comprehensive capabilities (corrections, causal links, indexed
	// string fields, text) + bulk volume.
	st, err := store.OpenOptions(logPath, store.Options{NoSync: true})
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	now := time.Now().UnixMicro()
	if _, err := demo.Seed(st, now); err != nil {
		log.Fatalf("demo seed: %v", err)
	}
	stats, err := synth.Seed(st, 800, 5, 3, rand.New(rand.NewSource(42)))
	if err != nil {
		log.Fatalf("bulk seed: %v", err)
	}
	st.Close()
	fmt.Printf("1) seeded   comprehensive demo + bulk %v\n", stats)

	// 2) ARCHIVE: compress + Merkle + zone maps.
	man, err := store.WriteArchive(logPath, arch, segMax)
	if err != nil {
		log.Fatalf("archive: %v", err)
	}
	var comp int64
	for _, s := range man.Segments {
		comp += s.Bytes
	}
	if fi, e := os.Stat(logPath); e == nil && fi.Size() > 0 {
		fmt.Printf("2) archived %d segment(s), %.0f%% of original size (compressed + tamper-evident)\n",
			len(man.Segments), 100*float64(comp)/float64(fi.Size()))
	}

	// 3) OPEN lazily: only current facts + zone maps resident.
	li, err := store.OpenLazyIndex(arch)
	if err != nil {
		log.Fatalf("open lazy: %v", err)
	}
	_ = li.SaveCheckpoint()
	fmt.Printf("3) lazy     %d live keys resident; indexed fields %v\n", li.Keys(), li.IndexedFields())

	// 4) RETRIEVE every way the lazy path supports.
	fmt.Println("\n-- RETRIEVE --")
	tsShow("current  sku:COFFEE-001/source", li.Current("sku:COFFEE-001", "source"))
	ev, idx := li.Lookup("category", "beverage")
	fmt.Printf("lookup   category=beverage (indexed=%v) -> %d fact(s)\n", idx, len(ev))
	if h, err := li.History("sku:MILK-002", "source"); err == nil {
		fmt.Printf("history  sku:MILK-002/source -> %d version(s) incl. correction\n", len(h))
	}
	if a, err := li.AsOf("sku:MILK-002", "source", now, 0); err == nil {
		tsShow("asof     sku:MILK-002/source (now)", a)
	}
	if hits, err := store.ScanSearch(arch, "beverage", 5); err == nil {
		fmt.Printf("search   'beverage' -> %d BM25 hit(s)\n", len(hits))
	}
	if cur := li.Current("sku:COFFEE-001", "source"); len(cur) > 0 {
		if tr, err := li.Trace(cur[0].EventID, "cause", 8); err == nil {
			fmt.Printf("trace    causes of current COFFEE price -> %d link(s)\n", len(tr))
		}
	}
	head, recs, verr := li.Verify()
	fmt.Printf("verify   %d records, chain head %s... -> %s\n", recs, tsPrefix(head, 12), tsOK(verr == nil))
	cs := li.CacheStats()
	fmt.Printf("cache    %d hit(s) / %d miss(es), %d segment(s) resident\n", cs.Hits, cs.Misses, cs.CachedSegments)

	// 5) INSERT: append a new fact + a correction, then re-archive.
	fmt.Println("\n-- INSERT --")
	st2, err := store.OpenOptions(logPath, store.Options{NoSync: true})
	if err != nil {
		log.Fatalf("reopen: %v", err)
	}
	t := time.Now().UnixMicro()
	newSku := &model.Event{Subject: "sku:TEA-NEW", Facet: "source", Type: model.Observed,
		EffectiveTime: t, Provenance: model.HumanEntry, Confidence: 1,
		Value: map[string]any{"price_cents": 899, "category": "beverage", "name": "loose leaf tea"}}
	corr := &model.Event{Subject: "sku:COFFEE-001", Facet: "source", Type: model.Correction,
		EffectiveTime: t, Provenance: model.ScanVerified, Confidence: 1,
		Value: map[string]any{"price_cents": 1599, "category": "beverage", "note": "shelf re-price"}}
	if err := st2.Append(t, []*model.Event{newSku, corr}, nil); err != nil {
		log.Fatalf("append: %v", err)
	}
	st2.Close()
	fmt.Println("inserted sku:TEA-NEW (new fact) + a COFFEE-001 price correction")

	if _, err := store.WriteArchive(logPath, arch, segMax); err != nil {
		log.Fatalf("re-archive: %v", err)
	}
	li2, err := store.OpenLazyIndex(arch) // stale checkpoint auto-invalidates (Merkle) and rebuilds
	if err != nil {
		log.Fatalf("reopen lazy: %v", err)
	}
	tsShow("current  sku:TEA-NEW/source (just inserted)", li2.Current("sku:TEA-NEW", "source"))
	tsShow("current  sku:COFFEE-001/source (after correction)", li2.Current("sku:COFFEE-001", "source"))
	if h, err := li2.History("sku:COFFEE-001", "source"); err == nil {
		fmt.Printf("history  sku:COFFEE-001/source -> now %d version(s)\n", len(h))
	}

	fmt.Println(strings.Repeat("-", 66))
	fmt.Printf("Explore it live:\n  centauri serve -lazy-index -data %s\nthen open http://localhost:7771  (Tablespace Console)\n", arch)
}

func tsShow(label string, evs []*model.Event) {
	if len(evs) == 0 {
		fmt.Printf("%s -> (none)\n", label)
		return
	}
	for _, e := range evs {
		fmt.Printf("%s -> %v\n", label, e.Value)
	}
}

func tsOK(ok bool) string {
	if ok {
		return "verified OK"
	}
	return "FAILED"
}

func tsPrefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// listenMaybeTLS serves over HTTPS when both a cert and key are given (native
// TLS, no reverse proxy required), otherwise plain HTTP.
func listenMaybeTLS(addr, cert, key string, h http.Handler) error {
	if cert != "" && key != "" {
		return http.ListenAndServeTLS(addr, cert, key, h)
	}
	return http.ListenAndServe(addr, h)
}

func authNote(token string) string {
	if token == "" {
		return ", UNAUTHENTICATED — set -read-token to require a token"
	}
	return ", token required"
}

// ensureDataPath makes pointing -data at a folder (e.g. a OneDrive directory)
// just work: a directory becomes <dir>/centauri.log, and parent dirs are
// created so a fresh path needs no mkdir. Keeps the database fully local and
// portable — drop it anywhere, including a synced or external drive.
func ensureDataPath(p string) string {
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		p = filepath.Join(p, "centauri.log")
	} else if strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`) {
		p = filepath.Join(p, "centauri.log")
	}
	if dir := filepath.Dir(p); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return p
}

// syncedFolderNote returns single-writer guidance when the data path lives in a
// cloud-sync folder, where two machines writing the same file at once would
// produce a sync conflict. Centauri's lock makes one machine the writer; use
// 'centauri sync' for genuine multi-device writes.
func syncedFolderNote(p string) string {
	low := strings.ToLower(filepath.ToSlash(p))
	for _, m := range []string{"onedrive", "dropbox", "google drive", "googledrive", "/icloud", "icloud drive", "box sync"} {
		if strings.Contains(low, m) {
			return "note: your data is in a synced folder — Centauri holds a single-writer lock, so THIS machine is the writer and the folder syncs it elsewhere as a backup. For live writes from another device run 'centauri sync' between the two; don't run 'serve' against the same file from two machines at once."
		}
	}
	return ""
}

// ollamaUp reports whether a local Ollama is already answering on its port.
func ollamaUp() bool {
	resp, err := (&http.Client{Timeout: 500 * time.Millisecond}).Get("http://localhost:11434")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// ensureOllama makes a local Ollama available for vision, the integrated-package
// way: if one is already running we use it as-is (and return nil, so we never
// stop someone else's). If not, and the binary is installed, we start
// `ollama serve` as a child and return its process so the caller can stop it on
// exit. If Ollama isn't installed we just point the user at setup.
func ensureOllama() *os.Process {
	if ollamaUp() {
		fmt.Println("vision: using the Ollama already running on :11434.")
		return nil
	}
	cmd := exec.Command("ollama", "serve")
	if err := cmd.Start(); err != nil {
		fmt.Printf("vision: couldn't start Ollama (%v) — start it yourself with 'ollama serve'.\n", err)
		return nil
	}
	// Don't block server startup waiting for it; Ollama warms up in parallel
	// and is ready well before the first ENRICH.
	fmt.Println("vision: started a local Ollama — it will stop when you close Centauri.")
	return cmd.Process
}

// runStream runs a command, echoing it and streaming its output, so the user
// sees exactly what's happening during setup.
func runStream(name string, args ...string) error {
	fmt.Printf("  $ %s %s\n", name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

// runSetupVision readies a local vision stack: a multimodal model server
// (Ollama) and a PDF rasteriser. With -install it uses the OS package manager
// to install missing pieces (explicit, echoed); otherwise it detects, pulls
// the Ollama models if Ollama is present, and prints exact next steps. It never
// touches the database — model registration is the one-click button in the UI.
func runSetupVision(install bool) {
	fmt.Print(banner)
	fmt.Println("Vision setup — getting a local AI ready to read your files")
	fmt.Println()
	goos := runtime.GOOS
	have := func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	rasteriser := func() string {
		for _, t := range []string{"pdftoppm", "pdftocairo", "magick", "convert"} {
			if have(t) {
				return t
			}
		}
		return ""
	}

	if install {
		fmt.Println("Installing missing prerequisites with your OS package manager (you may see a UAC/elevation prompt):")
		switch goos {
		case "windows":
			if !have("ollama") {
				_ = runStream("winget", "install", "-e", "--id", "Ollama.Ollama", "--silent", "--accept-package-agreements", "--accept-source-agreements")
			}
			if rasteriser() == "" {
				// ImageMagick can't decode PDFs without Ghostscript, so install both.
				_ = runStream("winget", "install", "-e", "--id", "ImageMagick.ImageMagick", "--silent", "--accept-package-agreements", "--accept-source-agreements")
				_ = runStream("winget", "install", "-e", "--id", "ArtifexSoftware.GhostScript", "--silent", "--accept-package-agreements", "--accept-source-agreements")
				fmt.Println("  (after install, open a NEW terminal so PATH updates take effect)")
			}
		case "darwin":
			if have("brew") {
				if !have("ollama") {
					_ = runStream("brew", "install", "ollama")
				}
				if rasteriser() == "" {
					_ = runStream("brew", "install", "poppler")
				}
			} else {
				fmt.Println("  Homebrew not found — install it from https://brew.sh, then re-run.")
			}
		default: // linux
			if rasteriser() == "" && have("apt-get") {
				_ = runStream("sudo", "apt-get", "install", "-y", "poppler-utils")
			}
			if !have("ollama") {
				fmt.Println("  Install Ollama:  curl -fsSL https://ollama.com/install.sh | sh")
			}
		}
		fmt.Println()
	}

	// Ollama: pull the models (this also starts the local server). Pulls only
	// run with -install; detect mode just reports (so it's a fast, passive
	// check callers like run-centauri.bat can run on every launch).
	if have("ollama") {
		if install {
			fmt.Println("Ollama found — pulling models (large one-time download; safe to leave running):")
			_ = runStream("ollama", "pull", "llava")
			_ = runStream("ollama", "pull", "nomic-embed-text")
		} else if modelPulled("llava") {
			fmt.Println("Ollama found; the 'llava' model is present.")
		} else {
			fmt.Println("Ollama found, but the 'llava' model isn't pulled yet.")
		}
	} else {
		fmt.Println("Ollama not found — the vision model server.")
		switch goos {
		case "windows":
			fmt.Println("  Install:  winget install Ollama.Ollama       (or https://ollama.com/download)")
		case "darwin":
			fmt.Println("  Install:  brew install ollama                (or https://ollama.com/download)")
		default:
			fmt.Println("  Install:  curl -fsSL https://ollama.com/install.sh | sh")
		}
	}

	if r := rasteriser(); r != "" {
		fmt.Printf("\nPDF rendering: ready (%s detected).\n", r)
	} else {
		fmt.Println("\nPDF rendering: not available yet (images already work).")
		switch goos {
		case "windows":
			fmt.Println("  Install:  winget install ImageMagick.ImageMagick ArtifexSoftware.GhostScript")
			fmt.Println("            (ImageMagick needs Ghostscript for PDFs; poppler/pdftoppm also works and needs neither)")
		case "darwin":
			fmt.Println("  Install:  brew install poppler")
		default:
			fmt.Println("  Install:  sudo apt-get install -y poppler-utils")
		}
	}

	fmt.Println("\nNext:")
	fmt.Println("  1) start Centauri:   centauri desktop")
	fmt.Println("  2) open  📎 Vision  →  click 'Register model:vision'")
	fmt.Println("  3) Upload an image/PDF  →  Run ENRICH  →  SEARCH it")

	ready := have("ollama") && modelPulled("llava") && rasteriser() != ""
	if !ready {
		if install {
			// winget/brew just put new tools on PATH that THIS process can't
			// see yet — a fresh shell is needed to finish (e.g. pull models).
			fmt.Println("\nIf tools were just installed, open a NEW terminal (or re-run run-centauri.bat) to finish — PATH updates aren't visible to the current session.")
		} else {
			fmt.Println("\nTip: 'centauri setup vision -install' installs/pulls whatever's missing.")
			// Non-zero exit lets run-centauri.bat detect "setup needed" and
			// offer a one-click install, so users never type the command.
			os.Exit(1)
		}
	}
}

// modelPulled reports whether an Ollama model has been downloaded.
func modelPulled(name string) bool {
	if _, err := exec.LookPath("ollama"); err != nil {
		return false
	}
	out, err := exec.Command("ollama", "list").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), name)
}
