// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (spec §3.1). Phase 4.7 runs the durable pipeline worker in-process;
// implementation and review execution are claimed over MCP. The
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
	"github.com/kidus-tiliksew/conveyor/internal/envfile"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func main() {
	if err := envfile.LoadDefault(); err != nil {
		log.Fatalf("load local environment: %v", err)
	}
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	configPath := flag.String("config", "conveyor.yaml", "path to deployment config")
	pollGitHub := flag.Duration("poll-github", 0, "poll interval for conveyor:ready issues (0 disables)")
	flag.Parse()

	deployment, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := deployment
	packBundle, err := pack.Load(deployment.PackDir)
	if err != nil {
		log.Fatalf("load Phase 3 pack: %v", err)
	}
	apiToken := os.Getenv("CONVEYOR_API_TOKEN")
	if apiToken == "" {
		log.Fatal("CONVEYOR_API_TOKEN is required for authenticated task creation")
	}
	pipelineKey := os.Getenv("CONVEYOR_API_KEY")
	if pipelineKey == "" {
		log.Fatal("CONVEYOR_API_KEY is required for in-process triage and spec stages")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	var pgStore *postgresstore.Store
	var closeStore func()
	switch deployment.Database.Backend {
	case "postgres":
		pgStore, err = postgresstore.Open(ctx, deployment.Database.URL)
		if err != nil {
			log.Fatalf("open Postgres store: %v", err)
		}
		if deployment.Workspace != "" {
			bootstrapCtx := store.WithWorkspace(ctx, deployment.Workspace)
			seeded, bootstrapErr := pgStore.BootstrapWorkspaceConfig(bootstrapCtx, deployment)
			if bootstrapErr != nil {
				pgStore.Close()
				log.Fatalf("bootstrap workspace: %v", bootstrapErr)
			}
			if !seeded {
				log.Printf("workspace %q already exists; ignoring workspace sections from %s", deployment.Workspace, *configPath)
			}
			cfg, err = pgStore.RuntimeConfig(bootstrapCtx, deployment)
			if err != nil {
				pgStore.Close()
				log.Fatalf("load database workspace config: %v", err)
			}
		} else {
			log.Printf("no bootstrap workspace configured; first-run workspace creation is available")
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
	agent := &inprocess.OpenAI{APIKey: pipelineKey, BaseURL: os.Getenv("CONVEYOR_API_BASE_URL")}
	d := dispatch.New(st, cfg, agent)
	d.Pack = packBundle
	var stopRiver func()
	var addWorkspaceQueue func(string) error
	if pgStore != nil {
		d.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
			return pgStore.RuntimeConfig(ctx, deployment)
		}
		d.UseDurableQueue()
		workspaceRecords, listErr := pgStore.ListWorkspaces(ctx)
		if listErr != nil {
			log.Fatalf("list workspaces: %v", listErr)
		}
		workspaceIDs := make([]string, 0, len(workspaceRecords))
		for _, item := range workspaceRecords {
			workspaceIDs = append(workspaceIDs, item.ID)
		}
		client, clientErr := dispatch.NewRiverClient(pgStore.Pool(), d, workspaceIDs)
		if clientErr != nil {
			log.Fatalf("create River worker: %v", clientErr)
		}
		if clientErr = client.Start(ctx); clientErr != nil {
			log.Fatalf("start River worker: %v", clientErr)
		}
		addWorkspaceQueue = func(workspace string) error { return dispatch.AddWorkspaceQueues(client, workspace) }
		stopRiver = func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := client.Stop(stopCtx); err != nil {
				log.Printf("stop River worker: %v", err)
			}
		}
		log.Printf("durable pipeline worker active; implementation/review available over MCP")
	} else {
		go d.Run(ctx)
	}
	if stopRiver != nil {
		defer stopRiver()
	}

	srv := httpapi.NewServer(st)
	srv.Repos = cfg.RepoNames()
	srv.Workspace = cfg.Workspace
	srv.WorkspaceInfo = httpapi.NewWorkspaceInfo(cfg)
	srv.Deployment = deployment
	srv.BearerToken = apiToken
	srv.OnCreate = d.Enqueue
	srv.OnIntervention = d.HandleIntervention
	srv.OnMerge = d.MergeApprovedTask
	srv.WorkOrders = &workorder.Service{Store: st, Dispatcher: d, Pack: packBundle, ConfigProvider: func(ctx context.Context) (*config.Config, error) {
		if pgStore != nil {
			return pgStore.RuntimeConfig(ctx, deployment)
		}
		return cfg, nil
	}}
	if pgStore != nil {
		srv.Workspaces = pgStore
		srv.EnsureWorkspaceQueues = addWorkspaceQueue
		srv.ConfigStore = pgStore
		srv.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
			return pgStore.RuntimeConfig(ctx, deployment)
		}
		reconcile := func() {
			workspaces, listErr := pgStore.ListWorkspaces(ctx)
			if listErr != nil {
				log.Printf("list workspaces for reconciliation: %v", listErr)
				return
			}
			for _, workspace := range workspaces {
				workspaceCtx := store.WithWorkspace(ctx, workspace.ID)
				repaired, err := pgStore.ReconcileQueuedTasks(workspaceCtx)
				if err != nil {
					log.Printf("reconcile queued tasks: %v", err)
					return
				}
				if repaired != 0 {
					log.Printf("reconciled %d queued task(s) missing River jobs", repaired)
				}
				publications, publicationErr := pgStore.ReconcileReviewPublications(workspaceCtx)
				if publicationErr != nil {
					log.Printf("reconcile review publications: %v", publicationErr)
					return
				}
				if publications != 0 {
					log.Printf("reconciled %d completed review publication(s) in workspace %s", publications, workspace.ID)
				}
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
		log.Printf("polling GitHub for %s issues in every workspace every %s", "conveyor:ready", *pollGitHub)
		go func() {
			ticker := time.NewTicker(*pollGitHub)
			defer ticker.Stop()
			for {
				if pgStore != nil {
					items, listErr := pgStore.ListWorkspaces(ctx)
					if listErr == nil {
						for _, item := range items {
							d.PollOnce(store.WithWorkspace(ctx, item.ID))
						}
					}
				} else if cfg.Workspace != "" {
					d.PollOnce(store.WithWorkspace(ctx, cfg.Workspace))
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
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
