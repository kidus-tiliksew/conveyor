// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (spec §3.1). Phase 1 runs the dispatcher and the local
// Docker runner in-process with an in-memory store; Phase 2 swaps in
// Postgres (event-sourced) + River, and the runner daemon splits out
// behind the runner protocol. The Phase 2+ dashboard SPA embeds here
// via go:embed so the whole control plane ships as one binary
// (spec §17.0).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/runner/localdocker"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	configPath := flag.String("config", "conveyor.yaml", "path to deployment config")
	pollGitHub := flag.Duration("poll-github", 0, "poll interval for conveyor:ready issues (0 disables)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	apiToken := os.Getenv("CONVEYOR_API_TOKEN")
	if apiToken == "" {
		log.Fatal("CONVEYOR_API_TOKEN is required for authenticated task creation")
	}

	st := store.NewMemory()
	d := dispatch.New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), localdocker.New(), cfg)

	srv := httpapi.NewServer(st)
	srv.Repos = cfg.RepoNames()
	srv.Workspace = cfg.Workspace
	srv.BearerToken = apiToken
	srv.OnCreate = d.Enqueue

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go d.Run(ctx)
	if *pollGitHub > 0 {
		log.Printf("polling GitHub for %s issues every %s", "conveyor:ready", *pollGitHub)
		go d.PollGitHub(ctx, *pollGitHub)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("conveyord listening on %s (workspace %s, %d repo(s))", *addr, cfg.Workspace, len(cfg.Repos))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
