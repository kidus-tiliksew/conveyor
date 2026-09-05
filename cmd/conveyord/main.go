// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (design-system-architecture). The durable pipeline worker runs in-process;
// implementation and review execution are claimed over MCP. The
// dashboard SPA embeds here so API and UI ship as one binary (design-system-architecture).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func main() {
	if handled, err := runServiceVerb(context.Background(), os.Args[1:], os.Stdout, os.Stderr); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := envfile.LoadDefault(); err != nil {
		log.Fatalf("load local environment: %v", err)
	}
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	configPath := flag.String("config", "conveyor.yaml", "path to deployment config")
	pollGitHub := flag.Duration("poll-github", 0, "poll interval for conveyor:ready issues (0 disables)")
	workerRetryDelay := flag.Duration("worker-retry-delay", workerservice.DefaultRetryDelay, "initial supervised-child retry delay")
	workerRetryMaximum := flag.Duration("worker-retry-max", workerservice.DefaultRetryMaximum, "maximum supervised-child retry delay")
	shutdownTimeoutFlag := flag.Duration("shutdown-timeout", defaultConveyordShutdownTimeout, "total graceful shutdown budget")
	flag.Parse()
	listenAddr, listenAddrSource, err := resolveConveyordListenAddress(*addr, flagWasSet(flag.CommandLine, "addr"), os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	shutdownTimeout, shutdownTimeoutSource, err := resolveConveyordShutdownTimeout(*shutdownTimeoutFlag, flagWasSet(flag.CommandLine, "shutdown-timeout"), os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if *workerRetryDelay <= 0 || *workerRetryMaximum < *workerRetryDelay {
		log.Fatal("worker retry delay must be positive and worker retry max must be at least the initial delay")
	}

	deployment, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	logControlPlaneModelOverrides(log.Printf)
	cfg := deployment
	packBundle, err := loadConveyordPack(deployment)
	if err != nil {
		log.Fatalf("load Phase 3 pack: %v", err)
	}
	planningRole, err := packBundle.PlanningRole()
	if err != nil {
		log.Fatalf("load planning role: %v", err)
	}
	apiToken := os.Getenv("CONVEYOR_API_TOKEN")
	if apiToken == "" {
		log.Fatal("CONVEYOR_API_TOKEN is required for authenticated task creation")
	}
	llmEnvironment, err := resolveConveyordLLMEnvironment(os.Getenv, log.Printf)
	if err != nil {
		log.Fatal(err)
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	httpBaseCtx, cancelHTTP := context.WithCancel(context.Background())
	defer cancelHTTP()

	st, err := backend.Open(ctx, deployment.Database)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if forgeKey, keyErr := config.ForgeTokenEncryptionKeyFromEnvironment(); keyErr != nil {
		log.Printf("forge token encryption unavailable until configured: %v", keyErr)
	} else {
		st.ConfigureForgeTokenEncryptionKey(forgeKey)
	}
	if _, bootstrapErr := st.BootstrapIdentity(ctx, config.FirstOperatorIdentityFromEnvironment(), apiToken); bootstrapErr != nil {
		st.Close()
		log.Fatalf("bootstrap deployment identity: %v", bootstrapErr)
	}
	if deployment.Workspace != "" {
		bootstrapCtx := store.WithWorkspace(ctx, deployment.Workspace)
		seeded, bootstrapErr := st.BootstrapWorkspaceConfig(bootstrapCtx, deployment)
		if bootstrapErr != nil {
			st.Close()
			log.Fatalf("bootstrap workspace: %v", bootstrapErr)
		}
		if !seeded {
			log.Printf("workspace %q already exists; ignoring workspace sections from %s", deployment.Workspace, *configPath)
		}
		cfg, err = st.RuntimeConfig(bootstrapCtx, deployment)
		if err != nil {
			st.Close()
			log.Fatalf("load database workspace config: %v", err)
		}
	} else {
		log.Printf("no bootstrap workspace configured; first-run workspace creation is available")
	}
	closeStore := st.Close
	closeStore = sync.OnceFunc(closeStore)
	defer closeStore()
	agent := &inprocess.OpenAI{APIKey: llmEnvironment.APIKey, BaseURL: llmEnvironment.BaseURL}
	agent.RedactionSecrets = st
	d := dispatch.New(st, cfg, agent)
	d.Pack = packBundle
	var queueRuntime shutdownQueue
	var addWorkspaceQueue func(string) error
	var workspaceQueues *dispatch.WorkspaceQueueRegistrar
	if st.IsDurable() {
		d.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
			return st.RuntimeConfig(ctx, deployment)
		}
		workspaceRecords, listErr := st.ListWorkspaces(ctx)
		if listErr != nil {
			log.Fatalf("list workspaces: %v", listErr)
		}
		workspaceIDs := make([]string, 0, len(workspaceRecords))
		for _, item := range workspaceRecords {
			workspaceIDs = append(workspaceIDs, item.ID)
		}
		workspaceConfigs := make(map[string]*config.Config, len(workspaceIDs))
		for _, workspaceID := range workspaceIDs {
			workspaceConfig, configErr := st.RuntimeConfig(store.WithWorkspace(ctx, workspaceID), deployment)
			if configErr != nil {
				log.Fatalf("load route config for workspace %s: %v", workspaceID, configErr)
			}
			workspaceConfigs[workspaceID] = workspaceConfig
		}
		rescueAfter, rescueErr := dispatch.QueueRescueThreshold(workspaceConfigs)
		if rescueErr != nil {
			log.Fatalf("validate queue rescue threshold: %v", rescueErr)
		}
		log.Printf("queue stuck-job rescue threshold %s", rescueAfter)
		queueShutdown := &dispatch.ShutdownMarker{}
		hostname, _ := os.Hostname()
		runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
			Workspaces: workspaceIDs, RescueStuckAfter: rescueAfter, WorkerID: hostname, Logf: log.Printf,
		})
		for _, registration := range d.Registrations(queueShutdown) {
			runtime.Register(registration)
		}
		if startErr := runtime.Start(context.Background()); startErr != nil {
			log.Fatalf("start queue runtime: %v", startErr)
		}
		workspaceQueues = dispatch.NewWorkspaceQueueRegistrar(workspaceIDs, func(workspace string) error {
			workspaceConfig, configErr := st.RuntimeConfig(store.WithWorkspace(ctx, workspace), deployment)
			if configErr != nil {
				return configErr
			}
			if configErr = dispatch.ValidateQueueRescueThreshold(workspace, workspaceConfig, rescueAfter); configErr != nil {
				return configErr
			}
			return runtime.EnsureWorkspace(workspace)
		}, log.Printf)
		addWorkspaceQueue = func(workspace string) error {
			_, err := workspaceQueues.Ensure(workspace)
			return err
		}
		queueRuntime = dispatch.NewMarkedRuntime(runtime, queueShutdown)
		log.Printf("durable pipeline worker active; implementation/review available over MCP")
	} else {
		go d.Run(ctx)
	}
	srv := httpapi.NewServer(st)
	srv.Credentials = st
	srv.Memberships = st
	srv.IdentityProvisioner = st
	srv.CallerIdentities = st
	srv.OwnProfiles = st
	srv.PersonalTokens = st
	srv.AgentCredentials = st
	srv.ForgeTokens = st
	srv.WorkspaceForgeTokens = st
	srv.InvitationSessions = st
	srv.Release = releaseinfo.Version
	srv.Repos = cfg.RepoNames()
	srv.Workspace = cfg.Workspace
	srv.WorkspaceInfo = httpapi.NewWorkspaceInfo(cfg)
	srv.Deployment = deployment
	srv.InvitationDelivery = config.InvitationDeliveryFromEnvironment()
	srv.BearerToken = apiToken
	srv.OnCreate = d.Enqueue
	srv.GenerateTaskTitle = d.GenerateTaskTitle
	srv.OnIntervention = d.HandleIntervention
	srv.OnMerge = d.MergeApprovedTask
	srv.OnMergeReadiness = d.ReadMergeReadiness
	srv.OnConflictFix = d.DispatchConflictFix
	srv.ValidateForgeToken = githubtrigger.ValidateTokenIdentity
	workOrders := &workorder.Service{Store: st, Dispatcher: d, Pack: packBundle, ConfigProvider: func(ctx context.Context) (*config.Config, error) {
		if st.IsDurable() {
			return st.RuntimeConfig(ctx, deployment)
		}
		return cfg, nil
	}}
	workOrders.ForgeTokens = st
	d.ForgeTokens = st
	workOrders.WorkspaceForgeTokens = st
	d.WorkspaceForgeTokens = st
	workOrders.RedactionSecrets = st
	srv.WorkOrders = workOrders
	srv.Planning = &planning.Service{
		Store: st, Agent: agent, ConfigProvider: workOrders.ConfigProvider,
		Prompt: planningRole,
	}
	{
		repositories := make(map[string]struct{}, len(cfg.Monitor.Repositories))
		for _, repository := range cfg.Monitor.Repositories {
			repositories[repository] = struct{}{}
		}
		srv.Monitor = &monitor.Service{
			Store: st, Intake: srv.CreateMonitorTask,
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
		srv.Monitor.RedactionSecrets = st
		d.ObserveDesignMerge = func(ctx context.Context, observation monitor.Observation, taskID string) error {
			_, err := srv.Monitor.ProcessDesignMerge(ctx, observation, taskID)
			return err
		}
	}
	srv.Workers = &workerservice.Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, RetryDelay: *workerRetryDelay, RetryMaximum: *workerRetryMaximum}
	srv.Workers.IdentityUsers = st
	srv.Workers.RedactionSecrets = st
	srv.Workers.ForgeTokens = st
	if st.IsDurable() {
		srv.Workspaces = st
		srv.EnsureWorkspaceQueues = addWorkspaceQueue
		srv.ConfigStore = st
		srv.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
			return st.RuntimeConfig(ctx, deployment)
		}
		reconcile := func() {
			workspaces, listErr := st.ListWorkspaces(ctx)
			if listErr != nil {
				log.Printf("list workspaces for reconciliation: %v", listErr)
				return
			}
			if queueErr := workspaceQueues.Converge(workspaces); queueErr != nil {
				log.Printf("%v", queueErr)
				return
			}
			for _, workspace := range workspaces {
				workspaceCtx := store.WithWorkspace(ctx, workspace.ID)
				mergeReadiness, mergeErr := d.ReconcileMergeReadiness(workspaceCtx)
				if mergeErr != nil {
					log.Printf("reconcile merge readiness: %v", mergeErr)
					return
				}
				if mergeReadiness != 0 {
					log.Printf("reconciled %d merge-ready task(s) in workspace %s", mergeReadiness, workspace.ID)
				}
				lifecycles, lifecycleErr := d.ReconcileGitHubLifecycles(workspaceCtx)
				if lifecycleErr != nil {
					log.Printf("reconcile GitHub lifecycle intents: %v", lifecycleErr)
					return
				}
				if lifecycles != 0 {
					log.Printf("reconciled %d approved task GitHub lifecycle intent(s) in workspace %s", lifecycles, workspace.ID)
				}
				issueJobs, issueErr := st.ReconcileGitHubLifecycles(workspaceCtx)
				if issueErr != nil {
					log.Printf("reconcile GitHub issue publications: %v", issueErr)
					return
				}
				if issueJobs != 0 {
					log.Printf("reconciled %d GitHub issue publication job(s) in workspace %s", issueJobs, workspace.ID)
				}
				repaired, err := st.ReconcileQueuedTasks(workspaceCtx)
				if err != nil {
					log.Printf("reconcile queued tasks: %v", err)
					return
				}
				if repaired != 0 {
					log.Printf("reconciled %d queued task(s) missing queue jobs", repaired)
				}
				closedBlueprints, closeErr := st.ReconcileBlueprintClosures(workspaceCtx)
				if closeErr != nil {
					log.Printf("reconcile blueprint closures: %v", closeErr)
					return
				}
				if closedBlueprints != 0 {
					log.Printf("reconciled %d blueprint parent closure(s) in workspace %s", closedBlueprints, workspace.ID)
				}
				publications, publicationErr := st.ReconcileReviewPublications(workspaceCtx)
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
				if st.IsDurable() {
					items, listErr := st.ListWorkspaces(ctx)
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
				if st.IsDurable() {
					items, listErr := st.ListWorkspaces(ctx)
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
						credential, tokenErr := st.GetWorkspaceForgeTokenForUse(workspaceCtx, workspaceID)
						if tokenErr != nil || strings.TrimSpace(credential.Token) == "" {
							_ = srv.Monitor.Store.RecordMonitorFailure(workspaceCtx, string(githubtrigger.ForgePermission),
								"workspace "+workspaceID+" forge token is unavailable; add or replace it in workspace settings", time.Now().Add(current.Monitor.PollInterval))
							continue
						}
						identity := "workspace " + workspaceID + " forge token"
						if credential.ForgeLogin != "" {
							identity += " for " + credential.ForgeLogin
						}
						runGitHub := githubtrigger.RESTRunner(credential.Token, identity)
						source := monitor.GitHubSource{
							WorkspaceID: workspaceID, Repository: repositoryName, GitHubSlug: repository.GitHub, Run: runGitHub,
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
							return monitor.FetchGitHubHints(ctx, repository.GitHub, revision, runGitHub)
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

	httpSrv := &http.Server{
		Addr: listenAddr, Handler: srv.Handler(),
		BaseContext: func(net.Listener) context.Context { return httpBaseCtx },
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-signalCtx.Done()
		conveyordShutdown{
			Timeout: shutdownTimeout, HTTP: httpSrv, Queue: queueRuntime,
			CancelHTTP: cancelHTTP, CancelService: cancelService, CloseStore: closeStore, Logf: log.Printf,
		}.Run()
		close(shutdownDone)
	}()

	log.Printf("conveyord listening on %s from %s (workspace %s, %d repo(s)); shutdown timeout %s from %s", listenAddr, listenAddrSource, cfg.Workspace, len(cfg.Repos), shutdownTimeout, shutdownTimeoutSource)
	if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-shutdownDone
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(current *flag.Flag) {
		if current.Name == name {
			set = true
		}
	})
	return set
}

func resolveConveyordListenAddress(flagAddr string, flagExplicit bool, getenv func(string) string) (string, string, error) {
	if flagExplicit {
		return flagAddr, "flag", nil
	}
	if value := getenv("CONVEYOR_LISTEN_ADDR"); value != "" {
		if _, _, err := net.SplitHostPort(value); err != nil {
			return "", "", fmt.Errorf("invalid CONVEYOR_LISTEN_ADDR %q: expected host:port: %w", value, err)
		}
		return value, "CONVEYOR_LISTEN_ADDR", nil
	}
	if value := getenv("PORT"); value != "" {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return "", "", fmt.Errorf("invalid PORT %q: expected a decimal port from 1 to 65535", value)
		}
		return net.JoinHostPort("0.0.0.0", value), "PORT", nil
	}
	return flagAddr, "default", nil
}

func loadConveyordPack(deployment *config.Config) (*pack.Bundle, error) {
	return pack.Load(deployment.PackDir)
}

func logControlPlaneModelOverrides(logf func(string, ...any)) {
	for _, override := range config.ActiveControlPlaneModelOverrides() {
		logf("control-plane model override active: %s=%s", override.Variable, override.Model)
	}
}

func resolveConveyordLLMEnvironment(getenv func(string) string, warnf func(string, ...any)) (config.LLMEnvironment, error) {
	environment := config.ResolveLLMEnvironment(getenv, warnf)
	if strings.TrimSpace(environment.APIKey) == "" {
		return config.LLMEnvironment{}, fmt.Errorf("CONVEYOR_LLM_API_KEY is required for in-process triage and spec stages (CONVEYOR_API_KEY is a deprecated fallback)")
	}
	return environment, nil
}
