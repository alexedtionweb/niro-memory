// Command niromem runs the memory engine as a service.
//
//	niromem serve   [-config file] [-dev-ui] [-mcp -mcp-user project]   HTTP API (+ inline worker)
//	niromem worker  [-config file] [-once N]   background jobs (-once: serverless)
//	niromem migrate [-config file]             create/upgrade the schema and exit
//	niromem rules test <rules.json> [text...]  validate a rules file and show matches
//	niromem keys hash <key>                    print the sha256 to put in a config
//	niromem bench   [-config file] [-n N]      measure latency/throughput on a synthetic dataset
//
// Without -config it runs a local development setup: SQLite at
// ./niromem.db, dev authentication (X-Tenant header), inline worker.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/metrics"
	"github.com/alexedtionweb/niro-memory/rules"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log, os.Args[2:])
	case "worker":
		err = worker(ctx, log, os.Args[2:])
	case "migrate":
		err = migrate(ctx, os.Args[2:])
	case "bench":
		err = bench(ctx, os.Args[2:])
	case "rules":
		err = rulesCmd(os.Args[2:])
	case "keys":
		if len(os.Args) == 4 && os.Args[2] == "hash" {
			sum := sha256.Sum256([]byte(os.Args[3]))
			fmt.Println(hex.EncodeToString(sum[:]))
			return
		}
		usage()
	default:
		usage()
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: niromem serve|worker|migrate|bench|rules test|keys hash   (see -h of each command)`)
	os.Exit(2)
}

func open(ctx context.Context, cfg *Config, log *slog.Logger, reg *metrics.Registry) (*niromem.Engine, func(), error) {
	o := niromem.Options{Logger: log}
	if reg != nil {
		o.Observer = reg
	}
	closers, err := connectors(ctx, cfg, &o)
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i].Close()
		}
	}
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	e, err := niromem.Open(o)
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return e, func() { e.Close(); closeAll() }, nil
}

func serve(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "", "config file (default: local development setup)")
	dev := fs.Bool("dev-ui", false, "serve the development console (requires -tags devui, development env, loopback address)")
	mcpEnabled := fs.Bool("mcp", false, "serve local MCP at /mcp in the same process and port as the API")
	mcpUser := fs.String("mcp-user", os.Getenv("NIROMEM_USER_ID"), "project-specific MCP user (or NIROMEM_USER_ID)")
	mcpAgent := fs.String("mcp-agent", os.Getenv("NIROMEM_AGENT_ID"), "MCP agent (or NIROMEM_AGENT_ID; default claude-code)")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if *dev {
		if err := devAllowed(cfg); err != nil {
			return err
		}
		cfg.knowledgeRuntime = true
	}
	reg := metrics.New()
	e, closeAll, err := open(ctx, cfg, log, reg)
	if err != nil {
		return err
	}
	defer closeAll()
	var auth api.Authenticator = api.Dev{}
	if !cfg.Auth.Dev {
		keys := make([]api.Key, len(cfg.Auth.Keys))
		for i, k := range cfg.Auth.Keys {
			keys[i] = api.Key(k)
		}
		if auth, err = api.NewKeys(keys); err != nil {
			return err
		}
	}
	handler := (&api.Server{Engine: e, Auth: auth, Metrics: reg, Logger: log}).Handler()
	if *mcpEnabled {
		if *mcpAgent == "" {
			*mcpAgent = "claude-code"
		}
		mcp, err := mcpHandler(e, auth, cfg.Listen, *mcpUser, *mcpAgent)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("/mcp", mcp)
		handler = mux
		log.Info("MCP enabled", "url", "http://"+cfg.Listen+"/mcp", "user", *mcpUser, "agent", *mcpAgent)
	}
	if *dev {
		if err := devAllowed(cfg); err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("/_dev/", devui.Handler(devui.Options{Engine: e, Auth: auth, Metrics: reg}))
		handler = mux
		log.Info("development console enabled", "url", "http://"+cfg.Listen+"/_dev/")
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	workerDone := make(chan struct{})
	wctx, cancelWorker := context.WithCancel(ctx)
	if cfg.Worker.Inline {
		w := e.Worker()
		w.Concurrency = cfg.Worker.Concurrency
		go func() { w.Run(wctx); close(workerDone) }()
	} else {
		close(workerDone)
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", cfg.Listen, "env", cfg.Env, "store", cfg.Store.Use, "capabilities", e.Capabilities())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		cancelWorker()
		return err
	case <-ctx.Done():
	}
	// Graceful shutdown: stop accepting, drain requests, then let running
	// jobs finish (they release their leases).
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	cancelWorker()
	<-workerDone
	return err
}

// devAllowed enforces that the console is development-only.
func devAllowed(cfg *Config) error {
	if !devui.Enabled {
		return errors.New("-dev-ui needs a binary built with -tags devui (make dev)")
	}
	if cfg.Env == "production" || os.Getenv("NIROMEM_ENV") == "production" {
		return errors.New("-dev-ui is refused in production")
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-dev-ui requires a loopback listen address, got %q", cfg.Listen)
	}
	return nil
}

func worker(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	path := fs.String("config", "", "config file")
	once := fs.Int("once", 0, "process at most N jobs and exit (serverless invocations)")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	e, closeAll, err := open(ctx, cfg, log, nil)
	if err != nil {
		return err
	}
	defer closeAll()
	w := e.Worker()
	w.Concurrency = cfg.Worker.Concurrency
	if *once > 0 {
		n, err := w.RunOnce(ctx, *once)
		log.Info("processed", "jobs", n)
		return err
	}
	log.Info("worker started", "id", w.Worker)
	return w.Run(ctx)
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	path := fs.String("config", "", "config file")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	_, closeAll, err := open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		return err
	}
	closeAll()
	fmt.Println("schema is up to date")
	return nil
}

func rulesCmd(args []string) error {
	if len(args) < 2 || args[0] != "test" {
		return errors.New("usage: niromem rules test <rules.json> [text ...]")
	}
	rs, err := rules.LoadFile(args[1])
	if err != nil {
		return err
	}
	f := rs.Rules()
	fmt.Printf("%s: %d facts, %d structured, %d entity, %d time, %d skip, %d flag rules; all examples pass\n",
		rs.Version, len(f.Facts), len(f.Structured), len(f.Entities), len(f.Times), len(f.Skip), len(f.Flags))
	text := strings.Join(args[2:], " ")
	if text == "" {
		return nil
	}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	return out.Encode(map[string]any{
		"candidates": rs.Text(text, rules.Context{User: "user", Role: "user", Observed: time.Now()}),
		"entities":   rs.Entities(text),
		"flags":      rs.Flags(text),
	})
}
