// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (component-runtime). The durable pipeline worker runs in-process;
// implementation and review execution are claimed over MCP. The
// dashboard SPA embeds here so API and UI ship as one binary (component-runtime).
package main

import (
	"context"
	"errors"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// conveyordFlags holds the daemon's flag values. registerDaemonFlags is the
// one definition of the daemon flag set, shared by main and the setup
// playbook's command-existence test (req-agent-guided-setup AC-5.3).
type conveyordFlags struct {
	addr, configPath, debugAddr                                       *string
	pollGitHub, workerRetryDelay, workerRetryMaximum, shutdownTimeout *time.Duration
	debugAllowRemote                                                  *bool
}

func registerDaemonFlags(flags *flag.FlagSet) conveyordFlags {
	return conveyordFlags{
		addr:               flags.String("addr", "127.0.0.1:8080", "listen address"),
		configPath:         flags.String("config", "conveyor.yaml", "path to deployment config"),
		pollGitHub:         flags.Duration("poll-github", 0, "poll interval for conveyor:ready issues (0 disables)"),
		workerRetryDelay:   flags.Duration("worker-retry-delay", workorder.DefaultReleaseRetryDelay, "initial automatic retry delay after a failed or stalled release"),
		workerRetryMaximum: flags.Duration("worker-retry-max", workorder.DefaultReleaseRetryMaximum, "maximum automatic retry delay after a failed or stalled release"),
		shutdownTimeout:    flags.Duration("shutdown-timeout", defaultConveyordShutdownTimeout, "total graceful shutdown budget"),
		debugAddr:          flags.String(debugAddrFlag, "", "opt-in net/http/pprof listen address (loopback only unless -"+debugAllowRemoteFlag+"; empty disables)"),
		debugAllowRemote:   flags.Bool(debugAllowRemoteFlag, false, "allow -"+debugAddrFlag+" to bind a non-loopback address"),
	}
}

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
	daemonFlags := registerDaemonFlags(flag.CommandLine)
	addr, configPath, pollGitHub := daemonFlags.addr, daemonFlags.configPath, daemonFlags.pollGitHub
	workerRetryDelay, workerRetryMaximum := daemonFlags.workerRetryDelay, daemonFlags.workerRetryMaximum
	shutdownTimeoutFlag, debugAddrValue, debugAllowRemote := daemonFlags.shutdownTimeout, daemonFlags.debugAddr, daemonFlags.debugAllowRemote
	flag.Parse()
	listenAddr, listenAddrSource, err := resolveConveyordListenAddress(*addr, flagWasSet(flag.CommandLine, "addr"), os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	shutdownTimeout, shutdownTimeoutSource, err := resolveConveyordShutdownTimeout(*shutdownTimeoutFlag, flagWasSet(flag.CommandLine, "shutdown-timeout"), os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	debugAddr, debugAddrSource, err := resolveConveyordDebugAddress(*debugAddrValue, flagWasSet(flag.CommandLine, debugAddrFlag), *debugAllowRemote, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if *workerRetryDelay <= 0 || *workerRetryMaximum < *workerRetryDelay {
		log.Fatal("worker retry delay must be positive and worker retry max must be at least the initial delay")
	}

	deployment, err := config.LoadDeployment(*configPath, log.Printf)
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
	gitHubAppKey, err := resolveConveyordGitHubAppKey(os.Getenv, log.Printf)
	if err != nil {
		log.Fatal(err)
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	// Startup, bootstrap, reconciliation, and every background loop derive
	// from the service context, so they record the explicit system actor. The
	// HTTP base context stays independent: a request records only its
	// credential-derived actor (component-persistence, Actor context).
	ctx = store.WithActor(ctx, store.SystemActor())
	httpBaseCtx, cancelHTTP := context.WithCancel(context.Background())
	defer cancelHTTP()

	st, dbStats, err := backend.OpenWithStats(ctx, deployment.Database)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if deployment.Database.Backend == "singlestore" {
		log.Printf("using durable SingleStore store with the event log")
	} else {
		log.Printf("using durable PostgreSQL store with the event log")
	}
	if dbStats.Pool != nil {
		// Read from the opened pool, so a pool_max_conns override shows.
		log.Printf("database pool max_conns=%d (effective)", dbStats.Pool().MaxConns)
	}
	if gitHubAppKey != nil {
		st.ConfigureGitHubAppKeyEncryptionKey(gitHubAppKey)
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
	// Call the latest callback: startup wraps it once the pool statistics
	// worker runs.
	defer func() { closeStore() }()
	agent := &inprocess.OpenAI{APIKey: llmEnvironment.APIKey, BaseURL: llmEnvironment.BaseURL}
	agent.RedactionSecrets = st
	d := dispatch.New(st, cfg, agent)
	d.Pack = packBundle
	var queueRuntime shutdownQueue
	var addWorkspaceQueue func(string, *config.Config) error
	var workspaceQueues *dispatch.WorkspaceQueueRegistrar
	startupChecks := &dispatch.StartupChecks{Dispatcher: d, Store: st, Logf: log.Printf}
	// startupWorkspaceIDs are the workspaces the start-up checks cover; a
	// workspace created after this read has no task to check.
	var startupWorkspaceIDs []string
	var reviewRecovery atomic.Pointer[workorder.Service]
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
		// One peer check serves the whole daemon on a backend without change
		// notification; the runtime, streams, and await_review keep no peer
		// timer and wake on the notices it republishes (component-runtime,
		// Peers; feature-quiet-idle-runtime, Peers).
		peerCheck := changes.SelectPeerCheck(ctx, deployment.Runtime.Peers, st, st.Changes(), logqueue.PeerTailInterval, log.Printf)
		// The order clock handler's review-round recovery pass reads the
		// work-order service once step 9 installs it (component-runtime,
		// Daemon startup order).
		d.AutomaticReviewRecovery = func(ctx context.Context, now time.Time) (int, error) {
			service := reviewRecovery.Load()
			if service == nil {
				return 0, nil
			}
			return service.SweepAutomaticReviewRecovery(ctx, now)
		}
		runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
			Workspaces: workspaceIDs, RescueStuckAfter: rescueAfter, WorkerID: hostname, Logf: log.Printf,
			Changes: st.Changes(), Deadlines: dispatch.StoreDeadlines(st),
		})
		for _, registration := range d.Registrations(queueShutdown) {
			runtime.Register(registration)
		}
		// The drift-repair start-up checks run before the runtime starts, so
		// no alarm fire or worker here repairs drift a check must report
		// (component-runtime, Start-up checks and background loops).
		startupWorkspaceIDs = workspaceIDs
		startupChecks.RunDrift(ctx, startupWorkspaceIDs)
		// The peer check's first statement runs before the runtime's first
		// catch-up and before the server serves, so every first read follows
		// the marks it holds; a failure is logged and the next check retries.
		if peerCheck != nil {
			_ = peerCheck.Check(ctx)
		}
		if startErr := runtime.Start(context.Background()); startErr != nil {
			log.Fatalf("start queue runtime: %v", startErr)
		}
		if peerCheck != nil {
			go peerCheck.Run(ctx)
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
		addWorkspaceQueue = func(workspace string, candidate *config.Config) error {
			// req-deployment-and-releases REQ-2: creation preflight runs before
			// persistence, so validate the candidate rather than reading its row.
			if err := dispatch.ValidateQueueRescueThreshold(workspace, candidate, rescueAfter); err != nil {
				return err
			}
			return runtime.EnsureWorkspace(workspace)
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
	srv.WorkspaceGitHubApps = st
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
	srv.OnGateMerge = d.MergeAtGate
	srv.OnMergeReadiness = d.ReadMergeReadiness
	srv.OnConflictFix = d.DispatchConflictFix
	workOrders := &workorder.Service{Store: st, Dispatcher: d, Pack: packBundle, ConfigProvider: func(ctx context.Context) (*config.Config, error) {
		if st.IsDurable() {
			return st.RuntimeConfig(ctx, deployment)
		}
		return cfg, nil
	}}
	workOrders.WorkspaceGitHubApps = st
	workOrders.PublicURL = srv.InvitationDelivery.PublicURL
	d.WorkspaceGitHubApps = st
	workOrders.RedactionSecrets = st
	srv.WorkOrders = workOrders
	srv.Planning = &planning.Service{
		Store: st, Agent: agent, ConfigProvider: workOrders.ConfigProvider,
		Prompt: planningRole,
	}
	if err := srv.Planning.CleanupSnapshots(ctx, cfg); err != nil {
		log.Fatalf("clean planning snapshots: %v", err)
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
	workOrders.RetryDelay, workOrders.RetryMaximum = *workerRetryDelay, *workerRetryMaximum
	// Every workspace's deadline alarm fires once with the review-round
	// recovery sweep in place (component-runtime, Daemon startup order).
	reviewRecovery.Store(workOrders)
	st.Changes().Publish(changes.Notice{Kind: changes.Deadline, At: time.Now().UTC()})
	if st.IsDurable() {
		srv.Workspaces = st
		srv.EnsureWorkspaceQueues = addWorkspaceQueue
		srv.ConfigStore = st
		srv.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
			return st.RuntimeConfig(ctx, deployment)
		}
		// Workspace queue registration converges at start, on creation, and
		// on each workspace notice, including a peer's; the start-up GitHub
		// reads run once. No repair pass runs on a timer (component-runtime,
		// Start-up checks and background loops; req-quiet-idle-runtime AC-4.3).
		startWorkspaceRegistrationWatcher(ctx, workspaceRegistrationWatcher{
			notices: st.Changes().SubscribeAll(changes.Workspace), list: st.ListWorkspaces,
			converge: workspaceQueues.Converge, logf: log.Printf,
		})
		startupChecks.RunGitHub(ctx, startupWorkspaceIDs)
	}

	// GitHub observation while running: webhook deliveries for every
	// workspace, the timed fallback for a workspace without webhook
	// delivery, and timed issue intake only with -poll-github. No monitor
	// loop runs (component-runtime, Start-up checks and background loops;
	// req-quiet-idle-runtime AC-3.1, AC-3.5, AC-3.7).
	listWorkspaceIDs := func(ctx context.Context) ([]string, error) {
		if !st.IsDurable() {
			if cfg.Workspace == "" {
				return nil, nil
			}
			return []string{cfg.Workspace}, nil
		}
		items, listErr := st.ListWorkspaces(ctx)
		if listErr != nil {
			return nil, listErr
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		return ids, nil
	}
	monitorPolls := &monitorPoller{Store: st, Dispatcher: d, Service: srv.Monitor, Config: workOrders.ConfigProvider}
	gitHubFallback := newGitHubFallback(st, d, listWorkspaceIDs, monitorPolls, log.Printf)
	webhookApplier := newGitHubWebhookApplier(st, d, monitorPolls.PushedRef, gitHubFallback.WebhookDelivered, log.Printf)
	webhookApplier.Start(ctx)
	srv.GitHubWebhooks = st
	srv.GitHubWebhookApplier = webhookApplier
	srv.GitHubSilenceFallback = gitHubFallback.SilenceActive
	fallbackWorkspaces := startupWorkspaceIDs
	if !st.IsDurable() {
		fallbackWorkspaces, _ = listWorkspaceIDs(ctx)
	}
	gitHubFallback.Start(ctx, fallbackWorkspaces)
	startIssueIntakePoll(ctx, *pollGitHub, listWorkspaceIDs, d.PollOnce, nil, log.Printf)

	if dbStats.Pool != nil || dbStats.Reads != nil {
		// The wrapped close stops and joins the worker, then logs the final
		// snapshot immediately before the store closes. SingleStore reports
		// read statistics only (component-runtime).
		ticker := time.NewTicker(poolStatsInterval)
		poolLogger := startPoolStatsLogger(ctx, dbStats, ticker.C, ticker.Stop, log.Printf)
		closeStore = sync.OnceFunc(poolLogger.closeWith(closeStore))
	}

	httpSrv := &http.Server{
		Addr: listenAddr, Handler: srv.Handler(),
		BaseContext: func(net.Listener) context.Context { return httpBaseCtx },
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	debugServer, err := startConveyordDebugServer(debugAddr, debugAddrSource, net.Listen, log.Printf)
	if err != nil {
		log.Fatal(err)
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-signalCtx.Done()
		conveyordShutdown{
			Timeout: shutdownTimeout, HTTP: httpSrv, Debug: debugServer, Queue: queueRuntime,
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

// logControlPlaneModelOverrides reports each active process-level model
// override with the in-process consumers it can reach. A line names an active
// variable, not the effective model: stage-specific overrides take precedence
// over the general one (component-runtime; component-triage).
func logControlPlaneModelOverrides(logf func(string, ...any)) {
	for _, override := range config.ActiveControlPlaneModelOverrides() {
		logf("control-plane model override active: %s=%s (%s)", override.Variable, override.Model, controlPlaneModelOverrideScope(override.Variable))
	}
}

func controlPlaneModelOverrideScope(variable string) string {
	switch variable {
	case config.TriageModelEnv:
		return "applies to triage and title generation"
	case config.PlanningModelEnv:
		return "applies to planning"
	case config.ControlPlaneModelEnv:
		return "fallback for in-process stages, title generation, and planning without a stage-specific override"
	default:
		return "in-process control-plane model"
	}
}

func resolveConveyordLLMEnvironment(getenv func(string) string, warnf func(string, ...any)) (config.LLMEnvironment, error) {
	environment := config.ResolveLLMEnvironment(getenv, warnf)
	if strings.TrimSpace(environment.APIKey) == "" {
		return config.LLMEnvironment{}, fmt.Errorf("CONVEYOR_LLM_API_KEY is required for in-process triage and spec stages (CONVEYOR_API_KEY is a deprecated fallback)")
	}
	return environment, nil
}

// resolveConveyordGitHubAppKey resolves the GitHub App key encryption key
// before the store opens (DEC-59 clause 2; req-delivery-and-forge AC-1.11).
// Conflicting canonical and deprecated values are fatal. A missing or
// malformed key stays nonfatal: the daemon logs that App key encryption is
// unavailable and returns no key, so App connection and use refuse until an
// operator configures it.
func resolveConveyordGitHubAppKey(getenv func(string) string, logf func(string, ...any)) ([]byte, error) {
	key, err := config.GitHubAppKeyEncryptionKeyFromEnvironment(getenv, logf)
	if errors.Is(err, config.ErrGitHubAppKeyEncryptionKeyConflict) {
		return nil, err
	}
	if err != nil {
		logf("GitHub App key encryption unavailable until configured: %v", err)
		return nil, nil
	}
	return key, nil
}

// DEC-59: production monitor polling resolves the same workspace app identity
// as dispatch and work-order reads, before constructing GitHubSource.
func workspaceMonitorGitHubRunner(ctx context.Context, st githubtrigger.AppStore, workspace, repo string, client *githubtrigger.AppClient) (func(context.Context, ...string) ([]byte, error), error) {
	token, err := client.WorkspaceToken(ctx, st, workspace)
	if err != nil {
		return nil, err
	}
	if err = client.RequireRepository(ctx, workspace, token, repo); err != nil {
		return nil, err
	}
	return githubtrigger.RESTRunner(token, githubtrigger.AppIdentity(workspace)), nil
}
