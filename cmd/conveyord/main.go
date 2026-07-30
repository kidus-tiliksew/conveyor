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
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"github.com/kidus-tiliksew/conveyor/internal/worktreemaint"
)

func main() {
	if err := envfile.LoadDefault(); err != nil {
		log.Fatalf("load local environment: %v", err)
	}
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	configPath := flag.String("config", "conveyor.yaml", "path to deployment config")
	pollGitHub := flag.Duration("poll-github", 0, "poll interval for conveyor:ready issues (0 disables)")
	workerRetryDelay := flag.Duration("worker-retry-delay", workerservice.DefaultRetryDelay, "initial supervised-child retry delay")
	workerRetryMaximum := flag.Duration("worker-retry-max", workerservice.DefaultRetryMaximum, "maximum supervised-child retry delay")
	flag.Parse()
	if *workerRetryDelay <= 0 || *workerRetryMaximum < *workerRetryDelay {
		log.Fatal("worker retry delay must be positive and worker retry max must be at least the initial delay")
	}

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
		st = store.NewMemoryWithConfig(cfg)
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
	srv.GenerateTaskTitle = d.GenerateTaskTitle
	srv.OnIntervention = d.HandleIntervention
	srv.OnMerge = d.MergeApprovedTask
	srv.OnMergeReadiness = d.ReadMergeReadiness
	srv.OnConflictFix = d.DispatchConflictFix
	workOrders := &workorder.Service{Store: st, Dispatcher: d, Pack: packBundle, ConfigProvider: func(ctx context.Context) (*config.Config, error) {
		if pgStore != nil {
			return pgStore.RuntimeConfig(ctx, deployment)
		}
		return cfg, nil
	}}
	srv.WorkOrders = workOrders
	srv.Planning = &planning.Service{
		Store: st, Agent: agent, ConfigProvider: workOrders.ConfigProvider,
		FinalizeBlueprint: d.CreatePlanningBlueprint,
	}
	startDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve daemon working directory: %v", err)
	}
	worktreeMaintainer := &worktreemaint.Maintainer{
		Store: st, ConfigProvider: workOrders.ConfigProvider, StartDir: startDir, Logf: log.Printf,
	}
	reconcileWorktrees := func() {
		var workspaceIDs []string
		if pgStore != nil {
			items, listErr := pgStore.ListWorkspaces(ctx)
			if listErr != nil {
				log.Printf("list workspaces for worktree maintenance: %v", listErr)
				return
			}
			for _, item := range items {
				workspaceIDs = append(workspaceIDs, item.ID)
			}
		} else if cfg.Workspace != "" {
			workspaceIDs = append(workspaceIDs, cfg.Workspace)
		}
		for _, workspaceID := range workspaceIDs {
			workspaceCtx := store.WithWorkspace(ctx, workspaceID)
			result, maintenanceErr := worktreeMaintainer.Reconcile(workspaceCtx)
			if maintenanceErr != nil {
				log.Printf("worktree maintenance workspace %s: %v", workspaceID, maintenanceErr)
				continue
			}
			if result.Cleaned != 0 {
				log.Printf("cleaned %d terminal task worktree(s) in workspace %s", result.Cleaned, workspaceID)
			}
		}
	}
	reconcileWorktrees()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileWorktrees()
			}
		}
	}()
	if monitorStore, ok := st.(monitor.Store); ok {
		repositories := make(map[string]struct{}, len(cfg.Monitor.Repositories))
		for _, repository := range cfg.Monitor.Repositories {
			repositories[repository] = struct{}{}
		}
		srv.Monitor = &monitor.Service{
			Store: monitorStore, Intake: srv.CreateMonitorTask,
			WorkspaceID: cfg.Workspace, Enabled: cfg.Monitor.Enabled,
			Repositories: repositories,
			ResolveScope: func(ctx context.Context) (string, bool, map[string]struct{}, error) {
				current, configErr := workOrders.ConfigProvider(ctx)
				if configErr != nil {
					return "", false, nil, configErr
				}
				scoped := make(map[string]struct{}, len(current.Monitor.Repositories))
				for _, repository := range current.Monitor.Repositories {
					scoped[repository] = struct{}{}
				}
				return current.Workspace, current.Monitor.Enabled, scoped, nil
			},
		}
	}
	srv.Workers = &workerservice.Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, RetryDelay: *workerRetryDelay, RetryMaximum: *workerRetryMaximum}
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
				lifecycles, lifecycleErr := d.ReconcileGitHubLifecycles(workspaceCtx)
				if lifecycleErr != nil {
					log.Printf("reconcile GitHub lifecycle intents: %v", lifecycleErr)
					return
				}
				if lifecycles != 0 {
					log.Printf("reconciled %d approved task GitHub lifecycle intent(s) in workspace %s", lifecycles, workspace.ID)
				}
				issueJobs, issueErr := pgStore.ReconcileGitHubLifecycles(workspaceCtx)
				if issueErr != nil {
					log.Printf("reconcile GitHub issue publications: %v", issueErr)
					return
				}
				if issueJobs != 0 {
					log.Printf("reconciled %d GitHub issue publication job(s) in workspace %s", issueJobs, workspace.ID)
				}
				repaired, err := pgStore.ReconcileQueuedTasks(workspaceCtx)
				if err != nil {
					log.Printf("reconcile queued tasks: %v", err)
					return
				}
				if repaired != 0 {
					log.Printf("reconciled %d queued task(s) missing River jobs", repaired)
				}
				closedBlueprints, closeErr := pgStore.ReconcileBlueprintClosures(workspaceCtx)
				if closeErr != nil {
					log.Printf("reconcile blueprint closures: %v", closeErr)
					return
				}
				if closedBlueprints != 0 {
					log.Printf("reconciled %d blueprint parent closure(s) in workspace %s", closedBlueprints, workspace.ID)
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

	if srv.Monitor != nil {
		go func() {
			lastPoll := map[string]time.Time{}
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				var workspaceIDs []string
				if pgStore != nil {
					items, listErr := pgStore.ListWorkspaces(ctx)
					if listErr != nil {
						log.Printf("list workspaces for monitor: %v", listErr)
					} else {
						for _, item := range items {
							workspaceIDs = append(workspaceIDs, item.ID)
						}
					}
				} else if cfg.Workspace != "" {
					workspaceIDs = append(workspaceIDs, cfg.Workspace)
				}
				for _, workspaceID := range workspaceIDs {
					workspaceCtx := store.WithWorkspace(ctx, workspaceID)
					current, configErr := workOrders.ConfigProvider(workspaceCtx)
					if configErr != nil || !current.Monitor.Enabled {
						continue
					}
					if previous := lastPoll[workspaceID]; !previous.IsZero() && time.Since(previous) < current.Monitor.PollInterval {
						continue
					}
					lastPoll[workspaceID] = time.Now()
					for _, repositoryName := range current.Monitor.Repositories {
						repository, ok := current.Repo(repositoryName)
						if !ok || repository.GitHub == "" {
							_ = srv.Monitor.Store.RecordMonitorFailure(workspaceCtx, "forge_response",
								"monitored repository requires a GitHub slug", time.Now().Add(current.Monitor.PollInterval))
							continue
						}
						source := monitor.GitHubSource{
							WorkspaceID: workspaceID, Repository: repositoryName, GitHubSlug: repository.GitHub,
							KnownLineage: func(taskID string, pullRequestNumber int, headSHA string) bool {
								task, taskErr := st.GetTask(workspaceCtx, taskID)
								if taskErr != nil || task.Repo != repositoryName ||
									task.Branch != "conveyor/task-"+taskID ||
									(task.GitHub != nil && task.GitHub.Repository != repository.GitHub) {
									return false
								}
								events, eventErr := st.ListEvents(workspaceCtx, taskID)
								if eventErr != nil {
									return false
								}
								return monitor.RecordedLineage(task, events, repositoryName, repository.GitHub,
									taskID, pullRequestNumber, headSHA)
							},
						}
						source.OnSuppressed = func(ctx context.Context, payload map[string]any) error {
							return srv.Monitor.Store.AuditMonitor(ctx, "monitor.suppressed", payload)
						}
						source.LoadHints = func(ctx context.Context, revision string) (*monitor.HintContext, error) {
							return monitor.FetchGitHubHints(ctx, repository.GitHub, revision, nil)
						}
						poller := monitor.Poller{
							Service: srv.Monitor, Source: source, StartupWindow: current.Monitor.StartupWindow,
							RetryInitial: time.Second, RetryMaximum: 30 * time.Second,
							Sleep: func(ctx context.Context, delay time.Duration) error {
								select {
								case <-ctx.Done():
									return ctx.Err()
								case <-time.After(delay):
									return nil
								}
							},
						}
						if pollErr := poller.Poll(workspaceCtx); pollErr != nil {
							log.Printf("monitor workspace %s repository %s: %v", workspaceID, repositoryName, pollErr)
						}
					}
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
