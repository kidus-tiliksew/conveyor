// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (spec §3.1). Postgres-backed Phase 2 runs only the control plane;
// conveyor-runner owns Docker and claims River work independently. The
// dashboard SPA embeds here so API and UI ship as one binary (spec §17.0).
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
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/runner/localdocker"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
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
	packBundle, err := pack.Load(cfg.PackDir)
	if err != nil {
		log.Fatalf("load Phase 3 pack: %v", err)
	}
	apiToken := os.Getenv("CONVEYOR_API_TOKEN")
	if apiToken == "" {
		log.Fatal("CONVEYOR_API_TOKEN is required for authenticated task creation")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	var pgStore *postgresstore.Store
	var closeStore func()
	switch cfg.Database.Backend {
	case "postgres":
		pgStore, err = postgresstore.Open(ctx, cfg.Database.URL)
		if err != nil {
			log.Fatalf("open Postgres store: %v", err)
		}
		if err := pgStore.BootstrapConfig(ctx, cfg); err != nil {
			pgStore.Close()
			log.Fatalf("bootstrap workspace: %v", err)
		}
		st = pgStore
		closeStore = pgStore.Close
		log.Printf("using durable Postgres store with River schema")
	case "memory":
		st = store.NewMemory()
		closeStore = func() {}
		log.Printf("WARNING: using volatile memory store; set CONVEYOR_DATABASE_URL for Phase 2 durability")
	}
	defer closeStore()
	var d *dispatch.Dispatcher
	if pgStore != nil {
		// Durable task mutations enqueue River transactionally. Execution-plane
		// access stays in the standalone conveyor-runner process.
		d = dispatch.New(st, nil, nil, cfg)
		d.UseDurableQueue()
		log.Printf("standalone conveyor-runner required for execution")
	} else {
		localRunner := localdocker.New()
		localRunner.SecretResolver = cfg.SecretResolver()
		localRunner.SecretPolicies = cfg.SecretPolicies()
		d = dispatch.New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), localRunner, cfg)
		credential := routing.Credential{
			ID: "memory-local-codex", OwnerID: cfg.Routing.OwnerID, OwnerKind: "user",
			Kind: "personal_sub", Vendor: "openai", Harness: "codex", Ref: cfg.CodexCredentials,
		}
		if len(cfg.Credentials) != 0 {
			configured := cfg.Credentials[0]
			credential = routing.Credential{
				ID: configured.ID, OwnerID: configured.OwnerID, OwnerKind: configured.OwnerKind,
				Kind: configured.Kind, Vendor: configured.Vendor, Harness: configured.Harness, Ref: configured.Ref,
			}
		}
		d.Router = routing.NewStatic(credential, cfg.Routing)
		go d.Run(ctx)
	}
	d.Pack = packBundle

	srv := httpapi.NewServer(st)
	srv.Repos = cfg.RepoNames()
	srv.Workspace = cfg.Workspace
	srv.BearerToken = apiToken
	srv.OnCreate = d.Enqueue
	srv.OnIntervention = d.HandleIntervention
	if pgStore != nil {
		reconcile := func() {
			repaired, err := pgStore.ReconcileQueuedTasks(ctx)
			if err != nil {
				log.Printf("reconcile queued tasks: %v", err)
				return
			}
			if repaired != 0 {
				log.Printf("reconciled %d queued task(s) missing River jobs", repaired)
			}
		}
		reconcile()
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					reconcile()
				}
			}
		}()
	}

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
