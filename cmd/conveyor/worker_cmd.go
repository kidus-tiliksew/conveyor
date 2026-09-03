package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/spf13/cobra"
)

var workerAttemptCheckpointer = checkpointAssignedTaskWorktree

func workerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Enroll and run the operator-owned worker dispatcher"}
	var ttl time.Duration
	pair := &cobra.Command{Use: "pair", Short: "Issue a short-lived single-use pairing token", RunE: func(cmd *cobra.Command, _ []string) error {
		token, expires, err := newClient().issueWorkerPairing(ttl)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "pairing_token=%s\nexpires_at=%s\n", token, expires.Format(time.RFC3339))
		return nil
	}}
	pair.Flags().DurationVar(&ttl, "ttl", workerservice.DefaultPairingTTL, "pairing token lifetime")
	list := &cobra.Command{Use: "list", Short: "List enrolled workers and health", RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := newClient().listWorkers()
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}}
	revoke := &cobra.Command{Use: "revoke <worker-id>", Short: "Revoke an enrolled worker", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := newClient().revokeWorker(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "revoked worker %s\n", args[0])
		return nil
	}}
	var pairing, name string
	configPath := defaultLocalExecutionConfigPath()
	var once bool
	run := &cobra.Command{Use: "run", Short: "Heartbeat, claim queued work, and supervise configured harnesses", RunE: func(cmd *cobra.Command, _ []string) error {
		resolvedConfig, err := resolveLocalExecutionConfigPath(cmd, configPath)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runWorkerWithConfig(ctx, newClient(), pairing, name, once, resolvedConfig.Path)
	}}
	run.Flags().StringVar(&pairing, "pairing-token", "", "single-use token for first enrollment")
	run.Flags().StringVar(&name, "name", defaultWorkerName(), "worker display name")
	run.Flags().BoolVar(&once, "once", false, "process currently available work and exit")
	run.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	cmd.AddCommand(pair, list, revoke, run, workerInstallCmd(), workerUninstallCmd(), workerStatusCmd())
	return cmd
}

func defaultWorkerName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = runtime.GOOS
	}
	return host
}

type workerCredentialFile struct {
	Workspace  string `json:"workspace"`
	WorkerID   string `json:"worker_id"`
	Credential string `json:"credential"`
}

func credentialPath(workspace string) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if workspace == "" {
		workspace = "default"
	}
	return filepath.Join(root, "conveyor", "workers", workspace+".json"), nil
}

func loadOrEnrollWorker(c *client, pairing, name string) (workerCredentialFile, error) {
	if credential := strings.TrimSpace(os.Getenv("CONVEYOR_WORKER_TOKEN")); credential != "" {
		return workerCredentialFile{Workspace: c.workspace, Credential: credential}, nil
	}
	path, err := credentialPath(c.workspace)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if data, readErr := os.ReadFile(path); readErr == nil {
		var saved workerCredentialFile
		if json.Unmarshal(data, &saved) == nil && saved.Credential != "" {
			return saved, nil
		}
	}
	if strings.TrimSpace(pairing) == "" {
		return workerCredentialFile{}, fmt.Errorf("worker is not enrolled; pass --pairing-token from `conveyor worker pair`")
	}
	enrollment, err := c.enrollWorker(pairing, name)
	if err != nil {
		return workerCredentialFile{}, err
	}
	saved := workerCredentialFile{Workspace: enrollment.Worker.Workspace, WorkerID: enrollment.Worker.ID, Credential: enrollment.Credential}
	path, err = credentialPath(enrollment.Worker.Workspace)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return workerCredentialFile{}, err
	}
	data, _ := json.Marshal(saved)
	if err = os.WriteFile(path, data, 0o600); err != nil {
		return workerCredentialFile{}, err
	}
	c.workspace = enrollment.Worker.Workspace
	return saved, nil
}

type childResult struct {
	stage core.Stage
	err   error
}

type codexUsageTotals struct {
	TokensIn  int64
	TokensOut int64
}

// codexUsageCollector accepts only Codex's documented JSONL turn.completed
// event. It deliberately ignores every other child line instead of growing a
// general harness-output scraper.
type codexUsageCollector struct {
	mu      sync.Mutex
	pending []byte
	latest  *codexUsageTotals
}

func (c *codexUsageCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p...)
	for {
		newline := bytes.IndexByte(c.pending, '\n')
		if newline < 0 {
			if len(c.pending) > 64*1024 {
				c.pending = nil
			}
			return len(p), nil
		}
		line := bytes.TrimSpace(c.pending[:newline])
		c.pending = c.pending[newline+1:]
		var event struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "turn.completed" || event.Usage == nil || event.Usage.InputTokens < 0 || event.Usage.OutputTokens < 0 {
			continue
		}
		c.latest = &codexUsageTotals{TokensIn: event.Usage.InputTokens, TokensOut: event.Usage.OutputTokens}
	}
}

func (c *codexUsageCollector) Usage() (codexUsageTotals, bool) {
	if c == nil {
		return codexUsageTotals{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil {
		return codexUsageTotals{}, false
	}
	return *c.latest, true
}

func enableCodexJSONOutput(harness config.Harness, argv []string) ([]string, *codexUsageCollector) {
	if harness.Name != "codex" {
		return argv, nil
	}
	for _, arg := range argv {
		if arg == "--json" {
			return argv, &codexUsageCollector{}
		}
	}
	insertAt := len(argv)
	for index, arg := range argv[1:] {
		if arg == "exec" || arg == "--" {
			insertAt = index + 2
			break
		}
	}
	withJSON := make([]string, 0, len(argv)+1)
	withJSON = append(withJSON, argv[:insertAt]...)
	withJSON = append(withJSON, "--json")
	withJSON = append(withJSON, argv[insertAt:]...)
	return withJSON, &codexUsageCollector{}
}

func reportCodexUsageFallback(c *client, credential, orderID, sessionID string, order core.WorkOrder, usage *codexUsageCollector) {
	totals, ok := usage.Usage()
	if !ok || order.UsageReported {
		return
	}
	// The terminal Codex usage event is emitted after the agent's terminal MCP
	// call, so this is intentionally best effort and bounded independently of
	// the child lifecycle.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.reportWorkerFallbackUsageContext(ctx, credential, orderID, sessionID, totals.TokensIn, totals.TokensOut)
}

type workerReconnectPolicy struct {
	Initial time.Duration
	Maximum time.Duration
	Jitter  func(time.Duration) time.Duration
}

var defaultWorkerReconnectPolicy = workerReconnectPolicy{
	Initial: 250 * time.Millisecond,
	Maximum: 5 * time.Second,
	Jitter: func(delay time.Duration) time.Duration {
		// Jitter is operational rather than security-sensitive. Derive a bounded
		// +/-20% offset without introducing process-global pseudo-random state.
		span := delay / 5
		if span <= 0 {
			return delay
		}
		offset := time.Duration(time.Now().UnixNano()%int64(2*span+1)) - span
		return delay + offset
	},
}

func (p workerReconnectPolicy) wait(ctx context.Context, delay time.Duration) error {
	if p.Jitter != nil {
		delay = p.Jitter(delay)
	}
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p workerReconnectPolicy) next(delay time.Duration) time.Duration {
	if delay <= 0 {
		delay = p.Initial
	}
	next := delay * 2
	if next > p.Maximum {
		return p.Maximum
	}
	return next
}

func runWorker(ctx context.Context, c *client, pairing, name string, once bool) error {
	return runWorkerWithConfig(ctx, c, pairing, name, once, defaultLocalExecutionConfigPath())
}

func runWorkerWithConfig(ctx context.Context, c *client, pairing, name string, once bool, configPath string) error {
	return runWorkerWithPolicyAndConfig(ctx, c, pairing, name, once, defaultWorkerReconnectPolicy, configPath)
}

const workerShutdownGracePeriod = 10 * time.Second

const (
	workerHealthyProbeInterval = time.Minute
	workerProbeRetryInitial    = 5 * time.Second
	workerProbeRetryMaximum    = time.Minute
)

type workerHarnessProbeState struct {
	probe      core.HarnessProbe
	nextProbe  time.Time
	retryDelay time.Duration
}

type workerHarnessProbes struct {
	states map[string]workerHarnessProbeState
	run    func(context.Context, []workerservice.HarnessProbeTarget) []core.HarnessProbe
}

func newWorkerHarnessProbes() *workerHarnessProbes {
	return &workerHarnessProbes{states: map[string]workerHarnessProbeState{}, run: probeHarnessTargets}
}

func runWorkerWithPolicy(ctx context.Context, c *client, pairing, name string, once bool, reconnect workerReconnectPolicy) error {
	return runWorkerWithPolicyAndConfig(ctx, c, pairing, name, once, reconnect, defaultLocalExecutionConfigPath())
}

func runWorkerWithPolicyAndConfig(ctx context.Context, c *client, pairing, name string, once bool, reconnect workerReconnectPolicy, configPath string) error {
	// Validate before enrollment, heartbeat, listing, or claiming so an enabled
	// worker never advertises serviceability it cannot execute locally.
	if _, err := loadLocalExecutionSetup(configPath); err != nil {
		return err
	}
	if c.forgeTokenPreflight != nil {
		if strings.TrimSpace(c.token) == "" {
			return fmt.Errorf("CONVEYOR_API_TOKEN is required for worker enrollment and execution")
		}
		if err := c.preflightForgeToken(ctx, c.token); err != nil {
			return err
		}
	}
	saved, err := loadOrEnrollWorker(c, pairing, name)
	if err != nil {
		return err
	}
	if c.workspace == "" {
		c.workspace = saved.Workspace
	}
	credential := saved.Credential
	active := map[string]core.Stage{}
	cleanupTasks := map[string]workerservice.DispatchOrder{}
	completions := make(chan childResult, 1024)
	started := false
	var mu sync.Mutex
	var children sync.WaitGroup
	defer func() {
		if ctx.Err() != nil {
			waitForWorkerChildren(&children, workerShutdownGracePeriod)
		}
	}()
	retryDelay := reconnect.Initial
	harnessProbes := newWorkerHarnessProbes()
	for {
		setup, err := loadLocalExecutionSetup(configPath)
		if err != nil {
			return err
		}
		document := setup.WorkerDocument
		probes := harnessProbes.probe(ctx, document, time.Now().UTC())
		if _, err = c.heartbeatWorkerContext(ctx, credential, probes); err != nil {
			if retryErr := waitForWorkerReconnect(ctx, reconnect, &retryDelay, "heartbeat worker", err); retryErr != nil {
				return retryErr
			}
			continue
		}
		harnessProbes.acknowledgeTransitions()
		orders, err := c.listWorkerOrdersContext(ctx, credential)
		if err != nil {
			if retryErr := waitForWorkerReconnect(ctx, reconnect, &retryDelay, "poll work orders", err); retryErr != nil {
				return retryErr
			}
			continue
		}
		retryDelay = reconnect.Initial
		cleanupWorkerTaskWorktrees(ctx, c, credential, cleanupTasks, setup.Config, os.Stderr)
		implementLimit := document.Execution.ImplementConcurrency
		if implementLimit < 1 {
			implementLimit = 1
		}
		reviewLimit := document.Execution.ReviewConcurrency
		if reviewLimit < 1 {
			reviewLimit = 1
		}
		counts := map[core.Stage]int{}
		overflowSkipped := false
		mu.Lock()
		for _, stage := range active {
			counts[stage]++
		}
		mu.Unlock()
		for _, item := range orders {
			if !item.Order.Claimable {
				continue
			}
			mu.Lock()
			_, running := active[item.Order.ID]
			mu.Unlock()
			if running {
				continue
			}
			limit := implementLimit
			if item.Order.Stage == core.StageReview {
				limit = reviewLimit
			}
			if counts[item.Order.Stage] >= limit {
				continue
			}
			selected, selectErr := selectLocalWorkerDispatch(item, setup.Config, os.Stderr)
			if selectErr != nil {
				if logWorkerReviewSeatOverflow(os.Stderr, item, setup.Config, selectErr) {
					overflowSkipped = true
					continue
				}
				return localExecutionSetupRemedy(configPath, selectErr)
			}
			mu.Lock()
			active[selected.Order.ID] = selected.Order.Stage
			mu.Unlock()
			cleanupTasks[selected.Task.ID] = selected
			counts[selected.Order.Stage]++
			started = true
			children.Add(1)
			go func(order workerservice.DispatchOrder, firstActivityTimeout time.Duration, local *config.Config) {
				defer children.Done()
				childCtx := contextWithWorktreeRoot(ctx, local.WorktreeRoot)
				childCtx = contextWithLocalExecutionConfig(childCtx, local)
				err := runHarnessChildWithFirstActivityTimeout(childCtx, c, credential, order, firstActivityTimeout)
				completions <- childResult{stage: order.Order.Stage, err: err}
				mu.Lock()
				delete(active, order.Order.ID)
				mu.Unlock()
			}(selected, setup.FirstActivityTimeout, setup.Config)
		}
		mu.Lock()
		activeCount := len(active)
		mu.Unlock()
		if once && activeCount == 0 && (started || len(orders) == 0 || overflowSkipped) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-completions:
			if result.err != nil {
				fmt.Fprintln(os.Stderr, "worker child:", result.err)
			}
		case <-time.After(5 * time.Second):
		}
	}
}

func cleanupWorkerTaskWorktrees(ctx context.Context, c *client, credential string, tasks map[string]workerservice.DispatchOrder, local *config.Config, output io.Writer) {
	for taskID, item := range tasks {
		attempt, err := attemptTerminalWorktreeCleanup(ctx, c, credential, item, local)
		if err != nil {
			fmt.Fprintf(output, "worker worktree cleanup task %s: %v\n", taskID, err)
			continue
		}
		for _, warning := range attempt.Cleanup.ProcessWarnings {
			fmt.Fprintf(output, "worker worktree cleanup task %s: warning: %s\n", taskID, warning)
		}
		if attempt.Completed {
			delete(tasks, taskID)
		}
	}
}

func logWorkerReviewSeatOverflow(output io.Writer, item workerservice.DispatchOrder, local *config.Config, err error) bool {
	var capacity *reviewSeatCapacityError
	if !errors.As(err, &capacity) {
		return false
	}
	_, _ = fmt.Fprintf(output, "worker order %s: review round requires seat %d, but default setup %q configures %d seat(s); leaving order queued without claiming\n", item.Order.ID, capacity.Required, local.DefaultSetup, capacity.Configured)
	return true
}

type localExecutionConfigContextKey struct{}

func contextWithLocalExecutionConfig(ctx context.Context, local *config.Config) context.Context {
	return context.WithValue(ctx, localExecutionConfigContextKey{}, local)
}

func localExecutionConfigFromContext(ctx context.Context) *config.Config {
	local, _ := ctx.Value(localExecutionConfigContextKey{}).(*config.Config)
	return local
}

func resolveHarnessWorkingDirectory(ctx context.Context, local *config.Config, item workerservice.DispatchOrder) (string, error) {
	launcherDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve launcher directory: %w", err)
	}
	// Old rolling-upgrade and unit-test dispatch fixtures may predate repository
	// identity. Production dispatches always carry both fields; retaining the
	// launcher's directory for legacy fixtures avoids guessing an identity.
	if strings.TrimSpace(item.Task.Repo) == "" || strings.TrimSpace(item.Repository.URL) == "" {
		return launcherDirectory, nil
	}
	// Direct callers without a selected local setup are legacy test and
	// rolling-upgrade seams. Both production dispatch paths always attach the
	// loaded setup before a claim is created.
	if local == nil {
		return launcherDirectory, nil
	}

	var configuredErr error
	if repo, ok := local.Repo(item.Task.Repo); ok && strings.TrimSpace(repo.Checkout) != "" {
		configuredCheckout, resolveErr := gitx.ResolvePrimaryCheckout(ctx, repo.Checkout, item.Task.Repo, item.Repository.URL)
		if resolveErr == nil {
			resolveErr = gitx.VerifyRepositoryIdentity(ctx, configuredCheckout, item.Task.Repo, item.Repository.URL)
		}
		if resolveErr == nil {
			return filepath.Clean(configuredCheckout), nil
		}
		configuredErr = fmt.Errorf("configured checkout %q is not verified: %w", repo.Checkout, resolveErr)
	}

	discovered, discoveredErr := gitx.ResolvePrimaryCheckout(ctx, launcherDirectory, item.Task.Repo, item.Repository.URL)
	if discoveredErr == nil {
		if verifyErr := gitx.VerifyRepositoryIdentity(ctx, discovered, item.Task.Repo, item.Repository.URL); verifyErr == nil {
			return discovered, nil
		} else {
			discoveredErr = verifyErr
		}
	}
	detail := discoveredErr
	if configuredErr != nil {
		detail = errors.Join(configuredErr, discoveredErr)
	}
	return "", fmt.Errorf("cannot resolve a verified primary checkout for repository %q; add a repos: [{name, url, checkout}] entry to the local execution config, or launch from inside the repository: %w", item.Task.Repo, detail)
}

func waitForWorkerChildren(children *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		children.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func waitForWorkerReconnect(ctx context.Context, reconnect workerReconnectPolicy, delay *time.Duration, operation string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !transientWorkerError(err) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if reconnect.Initial <= 0 || reconnect.Maximum < reconnect.Initial {
		return fmt.Errorf("invalid worker reconnect policy")
	}
	fmt.Fprintf(os.Stderr, "worker reconnect: %s failed transiently: %v; retrying\n", operation, err)
	if waitErr := reconnect.wait(ctx, *delay); waitErr != nil {
		return waitErr
	}
	*delay = reconnect.next(*delay)
	return nil
}

func validateWorkerConfig(document workerservice.WorkerConfig) error {
	if strings.TrimSpace(document.Workspace) == "" {
		return fmt.Errorf("workspace is required")
	}
	firstActivityTimeout, err := time.ParseDuration(document.Execution.FirstActivityTimeoutText)
	if err != nil || firstActivityTimeout <= 0 {
		return fmt.Errorf("execution.first_activity_timeout must be a positive duration")
	}
	for _, stage := range []string{"spec", "implement", "review"} {
		route, ok := document.Routing.Stages[stage]
		if !ok || route.Execution != config.ExecutionMCP {
			continue
		}
		stageTimeout, parseErr := time.ParseDuration(route.TimeoutText)
		if parseErr != nil || stageTimeout <= 0 {
			return fmt.Errorf("routing stage %s: timeout must be a positive duration", stage)
		}
		if firstActivityTimeout >= stageTimeout {
			return fmt.Errorf("execution.first_activity_timeout must be shorter than %s execution timeout", stage)
		}
	}
	validate := func(harness config.Harness) error {
		if strings.TrimSpace(harness.Name) == "" || len(harness.Command) == 0 || len(harness.ProbeCommand) == 0 {
			return fmt.Errorf("harness %q requires name, command, and probe_command", harness.Name)
		}
		if harness.MCPTransport == config.MCPTransportEnvironment {
			if err := config.ValidateHarness(harness); err != nil {
				return fmt.Errorf("harness %q has an invalid environment MCP attachment", harness.Name)
			}
		}
		return nil
	}
	for _, harness := range document.Harnesses {
		if err := validate(harness); err != nil {
			return err
		}
	}
	for _, target := range document.ActiveHarnesses {
		if err := validate(target.Harness); err != nil {
			return err
		}
	}
	return nil
}

func probeHarnesses(ctx context.Context, harnesses []config.Harness) []core.HarnessProbe {
	targets := make([]workerservice.HarnessProbeTarget, 0, len(harnesses))
	for _, harness := range harnesses {
		targets = append(targets, workerservice.HarnessProbeTarget{Harness: harness, Fingerprint: workerservice.HarnessFingerprint(harness)})
	}
	return probeHarnessTargets(ctx, targets)
}

func probeWorkerConfig(ctx context.Context, document workerservice.WorkerConfig) []core.HarnessProbe {
	return probeHarnessTargets(ctx, workerHarnessProbeTargets(document))
}

func workerHarnessProbeTargets(document workerservice.WorkerConfig) []workerservice.HarnessProbeTarget {
	byFingerprint := map[string]workerservice.HarnessProbeTarget{}
	for _, harness := range document.Harnesses {
		fingerprint := workerservice.HarnessFingerprint(harness)
		byFingerprint[fingerprint] = workerservice.HarnessProbeTarget{Harness: harness, Fingerprint: fingerprint}
	}
	for _, target := range document.ActiveHarnesses {
		if target.Fingerprint == "" {
			target.Fingerprint = workerservice.HarnessFingerprint(target.Harness)
		}
		byFingerprint[target.Fingerprint] = target
	}
	targets := make([]workerservice.HarnessProbeTarget, 0, len(byFingerprint))
	for _, target := range byFingerprint {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Harness.Name != targets[j].Harness.Name {
			return targets[i].Harness.Name < targets[j].Harness.Name
		}
		return targets[i].Fingerprint < targets[j].Fingerprint
	})
	return targets
}

func (p *workerHarnessProbes) probe(ctx context.Context, document workerservice.WorkerConfig, now time.Time) []core.HarnessProbe {
	targets := workerHarnessProbeTargets(document)
	active := make(map[string]bool, len(targets))
	due := make([]workerservice.HarnessProbeTarget, 0, len(targets))
	for _, target := range targets {
		key := target.Harness.Name + "\x00" + target.Fingerprint
		active[key] = true
		state, ok := p.states[key]
		if !ok || !state.nextProbe.After(now) {
			due = append(due, target)
		}
	}
	for key := range p.states {
		if !active[key] {
			delete(p.states, key)
		}
	}
	var observed []core.HarnessProbe
	if len(due) > 0 {
		observed = p.run(ctx, due)
	}
	for _, result := range observed {
		key := result.Harness + "\x00" + result.Fingerprint
		prior, hadPrior := p.states[key]
		if hadPrior && prior.probe.Healthy != result.Healthy {
			if result.Healthy {
				result.Transition = "unhealthy_to_healthy"
			} else {
				result.Transition = "healthy_to_unhealthy"
			}
		}
		state := workerHarnessProbeState{probe: result, retryDelay: workerProbeRetryInitial}
		if result.Healthy {
			state.nextProbe = now.Add(workerHealthyProbeInterval)
		} else {
			if hadPrior && !prior.probe.Healthy && prior.retryDelay > 0 {
				state.retryDelay = prior.retryDelay * 2
				if state.retryDelay > workerProbeRetryMaximum {
					state.retryDelay = workerProbeRetryMaximum
				}
			}
			state.nextProbe = now.Add(state.retryDelay)
		}
		p.states[key] = state
	}
	result := make([]core.HarnessProbe, 0, len(targets))
	for _, target := range targets {
		key := target.Harness.Name + "\x00" + target.Fingerprint
		if state, ok := p.states[key]; ok {
			result = append(result, state.probe)
		}
	}
	return result
}

func (p *workerHarnessProbes) acknowledgeTransitions() {
	for key, state := range p.states {
		state.probe.Transition = ""
		p.states[key] = state
	}
}

func probeHarnessTargets(ctx context.Context, targets []workerservice.HarnessProbeTarget) []core.HarnessProbe {
	results := make(chan core.HarnessProbe, len(targets))
	var probes sync.WaitGroup
	for _, target := range targets {
		target := target
		probes.Add(1)
		go func() {
			defer probes.Done()
			harness := target.Harness
			timeout := harness.ProbeTimeout
			if timeout <= 0 {
				timeout, _ = time.ParseDuration(harness.ProbeTimeoutText)
			}
			if timeout <= 0 {
				timeout = 10 * time.Second
			}
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			command := exec.CommandContext(probeCtx, harness.ProbeCommand[0], harness.ProbeCommand[1:]...)
			output, err := command.CombinedOutput()
			cancel()
			message := strings.TrimSpace(string(output))
			if len(message) > 500 {
				message = message[:500]
			}
			if err != nil && message == "" {
				message = err.Error()
			}
			results <- core.HarnessProbe{Harness: harness.Name, Fingerprint: target.Fingerprint, Healthy: err == nil, Message: message, CheckedAt: time.Now().UTC()}
		}()
	}
	probes.Wait()
	close(results)
	result := make([]core.HarnessProbe, 0, len(targets))
	for probe := range results {
		result = append(result, probe)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Harness != result[j].Harness {
			return result[i].Harness < result[j].Harness
		}
		return result[i].Fingerprint < result[j].Fingerprint
	})
	return result
}

func runHarnessChildWithFirstActivityTimeout(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration) error {
	return runHarnessChildWithFirstActivityTimeoutAndOutput(ctx, c, credential, item, firstActivityTimeout, os.Stdout, os.Stderr)
}

func runHarnessChildWithFirstActivityTimeoutAndRunMode(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, runMode string) error {
	return runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentation(ctx, c, credential, item, firstActivityTimeout, runMode, nil)
}

func runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentation(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, runMode string, presentation *runOutputPresentation) error {
	return runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentationAndStderr(ctx, c, credential, item, firstActivityTimeout, runMode, presentation, nil)
}

// The stderr override exists for the attached run app: while a Bubble Tea
// program owns the terminal, the child's stderr must flow into the program —
// one raw writer around the frame corrupts the repaint.
func runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentationAndStderr(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, runMode string, presentation *runOutputPresentation, stderr io.Writer) error {
	if stderr == nil {
		stderr = os.Stderr
	}
	return runHarnessChildWithFirstActivityTimeoutAndOutputAndRunModeAndPresentation(ctx, c, credential, item, firstActivityTimeout, os.Stdout, stderr, runMode, presentation)
}

func runHarnessChildWithFirstActivityTimeoutAndOutput(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, stdout, stderr io.Writer) error {
	return runHarnessChildWithFirstActivityTimeoutAndOutputAndRunMode(ctx, c, credential, item, firstActivityTimeout, stdout, stderr, "")
}

func runHarnessChildWithFirstActivityTimeoutAndOutputAndRunMode(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, stdout, stderr io.Writer, runMode string) error {
	return runHarnessChildWithFirstActivityTimeoutAndOutputAndRunModeAndPresentation(ctx, c, credential, item, firstActivityTimeout, stdout, stderr, runMode, nil)
}

func runHarnessChildWithFirstActivityTimeoutAndOutputAndRunModeAndPresentation(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, firstActivityTimeout time.Duration, stdout, stderr io.Writer, runMode string, presentation *runOutputPresentation) error {
	if firstActivityTimeout <= 0 {
		return fmt.Errorf("first activity timeout must be positive")
	}
	stallTimeout := item.Harness.StallTimeout
	if stallTimeout == 0 && item.Harness.StallTimeoutText != "" {
		var parseErr error
		stallTimeout, parseErr = time.ParseDuration(item.Harness.StallTimeoutText)
		if parseErr != nil || stallTimeout < 0 {
			return fmt.Errorf("stall timeout must be 0 to disable or a positive duration")
		}
	}
	session, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate worker session: %w", err)
	}
	clientToken, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate worker client token: %w", err)
	}
	sessionID := "worker-" + session
	delivery, err := c.claimDispatchOrderContext(ctx, credential, item, sessionID, clientToken)
	if err != nil {
		return err
	}
	claimed := delivery.WorkOrder
	forgeToken := delivery.ForgeToken
	// Clear the delivery envelope immediately. The token remains only in the
	// attempt-local value and child Git environment until this function exits.
	delivery = workerservice.ClaimDelivery{}
	var gitEnvironment map[string]string
	if item.Dispatch == "worker" && strings.TrimSpace(item.Task.Repo) != "" && strings.TrimSpace(item.Repository.URL) != "" && strings.TrimSpace(forgeToken) == "" {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.releaseDispatchOrderContext(releaseCtx, credential, item, core.WorkOrderRelease{
			SessionID: sessionID,
			Outcome:   core.WorkOrderOutcomeReleased,
			Reason:    "worker claim response omitted the executing user forge token",
			Cause:     core.WorkOrderReleaseCauseSessionExit,
		})
		cancel()
		return fmt.Errorf("worker claim response omitted the executing user forge token")
	}
	activityTail := &boundedTailWriter{limit: workerActivitySnapshotLimit}
	activitySnapshot := func() *core.WorkOrderActivitySnapshotInput {
		content := activityTail.String()
		if content == "" {
			return nil
		}
		return &core.WorkOrderActivitySnapshotInput{Content: content}
	}
	launchEnvironment := continuationLaunchEnvironment(claimed, item.Dispatch, c.workspace, credential)
	continuationPlan := planContinuationLaunch(claimed, item.Harness, launchEnvironment)
	leaseExpiresAt := claimed.LeaseExpiresAt
	// Pre-start setup (temp directory, MCP config, spec checkout clone) can
	// outlast the claim lease, so renewal must begin at claim time rather than
	// child launch (design-260805-973cd4: renewal keeps the claim alive but never
	// extends the fixed execution window). Authority loss cancels setupCtx so
	// long-running setup steps abort instead of continuing unclaimed.
	setupCtx, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	renewal := startPreStartClaimRenewal(setupCtx, cancelSetup, c, credential, item, sessionID, leaseExpiresAt, activitySnapshot)
	defer renewal.Stop()
	childCredential := credential
	if item.Dispatch == "run" {
		issued, issueErr := c.issueTaskRunAgentCredentialContext(setupCtx, credential, item, sessionID)
		if issueErr != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.releaseDispatchOrderContext(releaseCtx, credential, item, core.WorkOrderRelease{SessionID: sessionID, Outcome: core.WorkOrderOutcomeReleased, Reason: "issue child agent credential failed", Cause: core.WorkOrderReleaseCauseSessionExit})
			cancel()
			return fmt.Errorf("issue child agent credential: %w", issueErr)
		}
		childCredential = issued.Value
		defer func() {
			revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if revokeErr := c.revokeTaskRunAgentCredentialContext(revokeCtx, credential, item, sessionID, issued.ID); revokeErr != nil {
				_, _ = fmt.Fprintf(stderr, "warning: revoke child agent credential: %v\n", revokeErr)
			}
		}()
	}
	var redactedStdout, redactedStderr *redact.Writer
	var failureTail *boundedTailWriter
	var transcriptSpool *boundedTranscriptSpool
	var terminalRenderer *harnessEventRenderer
	flushOutput := func() {
		if redactedStdout != nil {
			_ = redactedStdout.Flush()
		}
		if redactedStderr != nil {
			_ = redactedStderr.Flush()
		}
		if terminalRenderer != nil {
			_ = terminalRenderer.Flush()
		}
	}
	release := func(outcome, reason string, exitStatus *int) error {
		flushOutput()
		detail := ""
		if failureTail != nil {
			detail = failureTail.String()
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cause := core.WorkOrderReleaseCauseSessionExit
		if strings.HasPrefix(reason, "claim authority lost") {
			cause = core.WorkOrderReleaseCauseLeaseLoss
		}
		return c.releaseDispatchOrderContext(releaseCtx, credential, item, core.WorkOrderRelease{SessionID: sessionID, Outcome: outcome, Reason: reason, Cause: cause, ExitStatus: exitStatus, FailureDetail: detail})
	}
	checkpointAttempt := func(reason string) error {
		if item.Order.Stage != core.StageImplement {
			return nil
		}
		flushOutput()
		// Rolling-upgrade and unit-test dispatch fixtures may predate the
		// repository identity contract. They cannot safely identify a task
		// worktree, so preservation remains unavailable rather than guessing.
		if strings.TrimSpace(item.Task.Branch) == "" || strings.TrimSpace(item.Task.Repo) == "" || strings.TrimSpace(item.Repository.URL) == "" {
			return nil
		}
		checkpointCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		checkpointCtx = contextWithGitEnvironment(checkpointCtx, gitEnvironment)
		result, checkpointErr := workerAttemptCheckpointer(checkpointCtx, item.Task.Branch, item.Task.Repo, item.Repository.URL, attemptCheckpoint{
			AttemptID: claimed.AttemptID, WorkOrderID: item.Order.ID, TerminationReason: reason,
		})
		cancel()
		if checkpointErr != nil || result == nil {
			return checkpointErr
		}
		checkpoint := core.WorkOrderAttemptCheckpoint{
			SessionID: sessionID, AttemptID: claimed.AttemptID, TerminationReason: reason,
			CommitSHA: result.CommitSHA, PushResult: "pushed",
		}
		var transcript *core.WorkOrderAttemptTranscript
		if transcriptSpool != nil {
			content, truncated, readErr := transcriptSpool.Snapshot()
			if readErr != nil {
				_, _ = fmt.Fprintf(stderr, "warning: read attempt transcript: %v; continuing without transcript because capture is best-effort\n", readErr)
			} else {
				transcript = &core.WorkOrderAttemptTranscript{Content: content, Truncated: truncated}
			}
		}
		if transcript != nil {
			transcriptCtx, cancelTranscript := context.WithTimeout(context.Background(), 5*time.Second)
			transcriptErr := c.checkpointDispatchOrderAttemptContext(transcriptCtx, credential, item, checkpoint, transcript)
			cancelTranscript()
			if transcriptErr == nil {
				return nil
			}
			_, _ = fmt.Fprintf(stderr, "warning: upload attempt transcript: %v; retrying checkpoint without transcript because capture is best-effort\n", transcriptErr)
		}
		checkpointOnlyCtx, cancelCheckpointOnly := context.WithTimeout(context.Background(), 30*time.Second)
		err := c.checkpointDispatchOrderAttemptContext(checkpointOnlyCtx, credential, item, checkpoint, nil)
		cancelCheckpointOnly()
		if err != nil {
			return fmt.Errorf("checkpoint commit %s was pushed from %s, but its audit event is not durable; successor reconciliation required: %w", result.CommitSHA, result.Worktree, err)
		}
		return nil
	}
	releaseAfterCheckpoint := func(outcome, reason string, exitStatus *int) error {
		if checkpointErr := checkpointAttempt(reason); checkpointErr != nil {
			if redactedStderr != nil {
				_, _ = fmt.Fprintf(redactedStderr, "attempt checkpoint failed: %v\n", checkpointErr)
			} else {
				_, _ = fmt.Fprintf(stderr, "attempt checkpoint failed: %v\n", checkpointErr)
			}
		}
		return release(outcome, reason, exitStatus)
	}
	if runMode != "" {
		message := "conveyor run mode: " + runMode
		if err = c.reportDispatchProgressContext(ctx, credential, item, sessionID, message); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: report run mode progress: %v; continuing because progress telemetry is best-effort\n", err)
		}
	}
	if hook := workerPreStartTestHook; hook != nil {
		hook(setupCtx)
	}
	directory, err := os.MkdirTemp("", "conveyor-worker-")
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "create worker temp directory failed", nil)
		return err
	}
	defer func() {
		_ = makeCheckoutWritable(directory)
		_ = os.RemoveAll(directory)
	}()
	transcriptSpool, err = newBoundedTranscriptSpool(directory, workerAttemptTranscriptLimit)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: create attempt transcript spool: %v; continuing because capture is best-effort\n", err)
		transcriptSpool = nil
	} else {
		defer func() {
			if removeErr := transcriptSpool.Remove(); removeErr != nil {
				_, _ = fmt.Fprintf(stderr, "warning: remove attempt transcript spool: %v\n", removeErr)
			}
		}()
	}
	mcpConfig, err := prepareMCPConfig(directory, c.base, childCredential, item.Harness.MCPTransport)
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "prepare MCP config failed", nil)
		return err
	}
	coldPrompt := workerLaunchPrompt(claimed, c.workspace, sessionID)
	prompt := coldPrompt
	if continuationPlan.Resume {
		prompt = continuationRecoveryPrompt(prompt, claimed)
	}
	var effortArgv []string
	if item.Effort != "" {
		if item.Order.Stage == core.StageSpec || item.Order.Stage == core.StageImplement {
			effortArgv = append([]string(nil), item.EffortArgv...)
			if len(effortArgv) == 0 {
				_ = release(core.WorkOrderOutcomeReleased, "configured effort is unsupported by harness snapshot", nil)
				return fmt.Errorf("implementation harness snapshot %s has no argv for effort %s", item.Harness.Name, item.Effort)
			}
		} else {
			effortArgv = append([]string(nil), item.Harness.EffortArgs[item.Effort]...)
		}
		if len(effortArgv) == 0 {
			_ = release(core.WorkOrderOutcomeReleased, "configured effort is unsupported by harness snapshot", nil)
			return fmt.Errorf("harness %s does not support effort %s", item.Harness.Name, item.Effort)
		}
	}
	coldArgv := expandHarnessWithEffortArgv(item.Harness, item.Model, effortArgv, coldPrompt, mcpConfig)
	if len(coldArgv) == 0 {
		_ = release(core.WorkOrderOutcomeReleased, "empty harness command", nil)
		return fmt.Errorf("harness %s has an empty command", item.Harness.Name)
	}
	argv := coldArgv
	if continuationPlan.Resume {
		argv = expandHarnessWithEffortArgv(item.Harness, item.Model, effortArgv, prompt, mcpConfig)
		argv = appendContinuationResumeArgv(argv, item.Harness.ResumeCommand, claimed.ContinuationSessionID)
	}
	argv, codexUsage := enableCodexJSONOutput(item.Harness, argv)
	coldArgv, _ = enableCodexJSONOutput(item.Harness, coldArgv)
	childAddress := c.base
	if item.Harness.MCPTransport == config.MCPTransportEnvironment {
		childAddress = strings.TrimRight(c.base, "/") + "/mcp"
	}
	childValues := map[string]string{
		"CONVEYOR_API_TOKEN":     childCredential,
		"CONVEYOR_ADDR":          childAddress,
		"CONVEYOR_WORKSPACE":     c.workspace,
		"CONVEYOR_WORK_ORDER_ID": item.Order.ID,
		"CONVEYOR_SESSION_ID":    sessionID,
		"CONVEYOR_CLIENT_TOKEN":  clientToken,
		"CONVEYOR_WORKTREE_ROOT": worktreeRootFromContext(ctx),
		// The branch assignment travels with the dispatch so `conveyor
		// checkout` resolves it locally; worker credentials are valid on the
		// worker and MCP planes only, never on workspace REST reads, so a
		// child cannot look the task up itself (design-http-api).
		"CONVEYOR_TASK_ID":                 item.Task.ID,
		"CONVEYOR_TASK_BRANCH":             item.Task.Branch,
		"CONVEYOR_TASK_BASE_BRANCH":        item.Task.BaseBranch,
		"CONVEYOR_TASK_REPO":               item.Task.Repo,
		"CONVEYOR_TASK_REPO_URL":           item.Repository.URL,
		"CONVEYOR_CURRENT_ATTEMPT_ID":      claimed.AttemptID,
		"CONVEYOR_PREVIOUS_ATTEMPT_ID":     claimed.LastAttemptID,
		"CONVEYOR_PREVIOUS_ATTEMPT_REASON": claimed.LastFailureMessage,
	}
	if item.GitAuthor.Name != "" && item.GitAuthor.Email != "" {
		childValues["GIT_AUTHOR_NAME"] = item.GitAuthor.Name
		childValues["GIT_AUTHOR_EMAIL"] = item.GitAuthor.Email
		childValues["GIT_COMMITTER_NAME"] = item.GitAuthor.Name
		childValues["GIT_COMMITTER_EMAIL"] = item.GitAuthor.Email
	}
	gitEnvironment, err = childGitCredentialEnvironment(forgeToken)
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "prepare child Git credential environment failed", nil)
		return err
	}
	defer func() {
		for key := range gitEnvironment {
			gitEnvironment[key] = ""
		}
		forgeToken = ""
	}()
	workingDirectory := ""
	if item.Order.Stage == core.StageSpec {
		if len(gitEnvironment) > 0 {
			err = requireHTTPSRemote(item.Repository.URL)
		}
		if err != nil {
			_ = release(core.WorkOrderOutcomeReleased, err.Error(), nil)
			return err
		}
		workingDirectory, err = materializeSpecCheckout(contextWithGitEnvironment(setupCtx, gitEnvironment), directory, item)
		if err != nil {
			if _, lost := renewal.Stop(); lost != nil {
				if workerOrderPreempted(lost) {
					return errWorkerOrderPreempted
				}
				_ = release(core.WorkOrderOutcomeReleased, "claim authority lost: "+lost.Error(), nil)
				return lost
			}
			_ = release(core.WorkOrderOutcomeReleased, "materialize spec repository failed", nil)
			return err
		}
	} else {
		workingDirectory, err = resolveHarnessWorkingDirectory(setupCtx, localExecutionConfigFromContext(ctx), item)
		if err != nil {
			reason := err.Error()
			_ = release(core.WorkOrderOutcomeReleased, reason, nil)
			return err
		}
		if len(gitEnvironment) > 0 {
			err = validateHTTPSOrigin(setupCtx, workingDirectory)
		}
		if err != nil {
			_ = release(core.WorkOrderOutcomeReleased, err.Error(), nil)
			return err
		}
	}
	childEnv := isolatedChildEnvironment(os.Environ(), childValues)
	childEnv = isolatedChildEnvironment(childEnv, gitEnvironment)
	if item.Harness.MCPTransport == config.MCPTransportEnvironment {
		if err = validateEnvironmentAttachment(setupCtx, item.Harness, childEnv, workingDirectory); err != nil {
			if _, lost := renewal.Stop(); lost != nil {
				if workerOrderPreempted(lost) {
					return errWorkerOrderPreempted
				}
				_ = release(core.WorkOrderOutcomeReleased, "claim authority lost: "+lost.Error(), nil)
				return lost
			}
			_ = release(core.WorkOrderOutcomeReleased, "environment MCP readiness failed", nil)
			return err
		}
	}
	outputRedactor := redact.New([]string{credential, childCredential, childAddress, sessionID, clientToken, forgeToken})
	failureTail = &boundedTailWriter{limit: workerservice.FailureDetailLimit}
	var usageDestination io.Writer
	if codexUsage != nil {
		usageDestination = codexUsage
	}
	stdoutFanout, renderer := harnessStdoutFanout(stdout, failureTail, usageDestination, item, presentation)
	terminalRenderer = renderer
	if continuationObserverEnabled(item.Harness, launchEnvironment) {
		reporter := newContinuationReporter(c, credential, item, claimed, launchEnvironment, func(message string) {
			_, _ = fmt.Fprintln(stderr, "warning:", message)
		})
		defer reporter.Stop()
		observer := newContinuationSessionObserver(reporter.Observe)
		stdoutFanout = io.MultiWriter(stdoutFanout, observer)
	}
	observabilityDestinations := []io.Writer{activityTail}
	if transcriptSpool != nil {
		observabilityDestinations = append(observabilityDestinations, transcriptSpool)
	}
	stdoutFanout = io.MultiWriter(append([]io.Writer{stdoutFanout}, observabilityDestinations...)...)
	redactedStdout = &redact.Writer{Destination: stdoutFanout, Redactor: outputRedactor}
	redactedStderr = &redact.Writer{Destination: io.MultiWriter(append([]io.Writer{stderr, failureTail}, observabilityDestinations...)...), Redactor: outputRedactor}
	// Both redacted streams share one first-write signal; either stream
	// permanently disarms output-start liveness (design-260805-973cd4).
	firstActivity := newFirstActivitySignal()
	defer flushOutput()
	// Hand lease authority to the running-child loop: stop pre-start renewal
	// before launch so exactly one renewer owns the claim at a time.
	handoffLease, lost := renewal.Stop()
	if lost != nil {
		if ctx.Err() != nil {
			if item.Dispatch != "run" {
				_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			}
			return ctx.Err()
		}
		if workerOrderPreempted(lost) {
			return errWorkerOrderPreempted
		}
		_ = release(core.WorkOrderOutcomeReleased, "claim authority lost: "+lost.Error(), nil)
		return lost
	}
	leaseExpiresAt = handoffLease
	newCommand := func(commandArgv []string) *exec.Cmd {
		command := exec.CommandContext(ctx, commandArgv[0], commandArgv[1:]...)
		command.Stdout = &firstActivityWriter{Destination: redactedStdout, Signal: firstActivity}
		command.Stderr = &firstActivityWriter{Destination: redactedStderr, Signal: firstActivity}
		command.Env = childEnv
		command.Dir = workingDirectory
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return command
	}
	command, _, resumeStartErr, err := startHarnessCommandWithColdFallback(continuationPlan.Resume && ctx.Err() == nil, argv, coldArgv, newCommand)
	if resumeStartErr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: resume launch failed to start: %v; retrying cold\n", resumeStartErr)
		continuationPlan = continuationLaunchPlan{Reason: "resume_start_failed"}
	}
	if err != nil {
		if ctx.Err() != nil {
			if item.Dispatch != "run" {
				_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			}
			return ctx.Err()
		}
		_ = release(core.WorkOrderOutcomeChildFailure, "harness launch failed: "+err.Error(), nil)
		return err
	}
	if claimed.LastAttemptID != "" {
		auditMode := "cold"
		if continuationPlan.Resume {
			auditMode = "resumed"
		}
		auditCtx, cancel := context.WithTimeout(context.Background(), continuationReportTimeout)
		auditMessage := "conveyor continuation recovery: mode=" + auditMode + " reason=" + continuationPlan.Reason
		if auditErr := c.reportDispatchProgressContext(auditCtx, credential, item, sessionID, auditMessage); auditErr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: report continuation recovery progress: %v; continuing because progress telemetry is best-effort\n", auditErr)
		}
		cancel()
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	processGroup := harnessProcessGroup{pgid: command.Process.Pid, done: done}
	firstActivityTimer := time.NewTimer(firstActivityTimeout)
	defer firstActivityTimer.Stop()
	firstActivityDeadline := firstActivityTimer.C
	firstActivityObserved := firstActivity.observed
	var stallTimer *time.Timer
	var stallDeadline <-chan time.Time
	var stallGeneration uint64
	var stallDue time.Time
	if stallTimeout > 0 {
		stallTimer = time.NewTimer(stallTimeout)
		stallDeadline = stallTimer.C
		stallDue = time.Now().Add(stallTimeout)
		defer stallTimer.Stop()
	}
	activityObserved := firstActivity.activity
	renewEvery := workerClaimRenewInterval
	if remaining := time.Until(leaseExpiresAt); remaining > 0 && remaining/3 < renewEvery {
		renewEvery = remaining / 3
	}
	if renewEvery <= 0 {
		renewEvery = time.Millisecond
	}
	ticker := time.NewTicker(renewEvery)
	defer ticker.Stop()
	claimFinalized := false
	checkpointReleaseReason := ""
	var finalizedOrder core.WorkOrder
	var runTerminalTimer *time.Timer
	var runTerminalDeadline <-chan time.Time
	defer func() {
		if runTerminalTimer != nil {
			runTerminalTimer.Stop()
		}
	}()
	observeFinalized := func(order core.WorkOrder) {
		claimFinalized = true
		finalizedOrder = order
		firstActivityObserved = nil
		firstActivityDeadline = nil
		stallDeadline = nil
		if item.Dispatch == "run" && runTerminalTimer == nil {
			runTerminalTimer = time.NewTimer(workerRunTerminalChildGrace)
			runTerminalDeadline = runTerminalTimer.C
		}
	}
	observeCheckpointRelease := func(conflictErr error) (bool, error) {
		typedRelease := workerOrderReleasedAtCheckpoint(conflictErr)
		if !typedRelease && (item.Dispatch != "run" || !workerOrderConflict(conflictErr)) {
			return false, nil
		}
		if item.Dispatch == "run" {
			reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reconciled, reconcileErr := c.reconcileTaskRunOrderContext(reconcileCtx, credential, item, sessionID)
			cancel()
			if reconcileErr == nil && reconciled.ReleasedAtCheckpoint &&
				(reconciled.WorkOrder.LastFailureMessage == core.WorkOrderReleaseReasonOperatorCheckpointReached ||
					reconciled.WorkOrder.LastFailureMessage == core.WorkOrderReleaseReasonPlanRevisionRequested) {
				checkpointReleaseReason = reconciled.WorkOrder.LastFailureMessage
				observeFinalized(reconciled.WorkOrder)
				return true, nil
			}
			if typedRelease {
				// The typed response proves a deliberate self-release but not which
				// release reason was persisted. Fail closed when the read-only
				// reconciliation cannot supply that reason rather than relabeling a
				// plan-revision release as an operator checkpoint. The caller must
				// not attempt a second release for this already-released claim.
				if reconcileErr != nil {
					return false, fmt.Errorf("confirm checkpoint release reason: %w", reconcileErr)
				}
				return false, fmt.Errorf("confirm checkpoint release reason: server reported %s (%s)", reconciled.WorkOrder.State, reconciled.Reason)
			}
		}
		if typedRelease {
			checkpointReleaseReason = core.WorkOrderReleaseReasonOperatorCheckpointReached
			observeFinalized(core.WorkOrder{ID: item.Order.ID, State: core.WorkOrderQueued, LastFailureMessage: checkpointReleaseReason})
			return true, nil
		}
		return false, nil
	}
	presentCheckpointRelease := func() error {
		if item.Dispatch != "run" {
			return nil
		}
		if err := presentRunCheckpointPause(stdout, presentation, string(item.Order.Stage), checkpointReleaseReason); err != nil {
			return fmt.Errorf("present operator checkpoint pause: %w", err)
		}
		return nil
	}
	handleChildExit := func(waitErr error) error {
		waitErr = processGroup.terminate(&waitErr)
		flushOutput()
		if ctx.Err() != nil {
			if item.Dispatch != "run" {
				_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			}
			return ctx.Err()
		}
		if checkpointReleaseReason != "" {
			// Renewal already proved that this exact child durably released for
			// operator direction. The release is the successful stage handoff;
			// child exit status cannot turn it into claim loss or a duplicate
			// release (design-260805-973cd4).
			return presentCheckpointRelease()
		}
		var exitStatus *int
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				status := exitErr.ExitCode()
				exitStatus = &status
			}
		}
		// Reconcile before checkpointing so a terminal handoff that committed
		// just before child exit cannot receive a later WIP commit. A terminal
		// order already observed by renewal is authoritative and avoids making
		// successful completion depend on an older server's reconcile behavior.
		reconciled := workerservice.ClaimReconciliation{}
		var reconcileErr error
		if claimFinalized && (finalizedOrder.State == core.WorkOrderSubmitted || finalizedOrder.State == core.WorkOrderCompleted) {
			reconciled.WorkOrder = finalizedOrder
			reconciled.Reason = "renewal observed a durable terminal handoff"
		} else {
			reconciled, reconcileErr = reconcileDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt)
		}
		if reconcileErr != nil {
			observedCheckpoint, checkpointErr := observeCheckpointRelease(reconcileErr)
			if observedCheckpoint {
				return presentCheckpointRelease()
			}
			if checkpointErr != nil {
				return fmt.Errorf("confirm work-order completion: %w", checkpointErr)
			}
			_ = releaseAfterCheckpoint(core.WorkOrderOutcomeChildFailure, "could not confirm work-order completion", exitStatus)
			return fmt.Errorf("confirm work-order completion: %w", reconcileErr)
		}
		renewed := reconciled.WorkOrder
		// The service checks current persisted provenance before applying this
		// fallback, so a report emitted by the agent during the child run wins.
		reportCodexUsageFallback(c, credential, item.Order.ID, sessionID, renewed, codexUsage)
		if renewed.State == core.WorkOrderClaimed && reconciled.Authorized {
			reason := "harness exited before completing work order"
			if waitErr != nil {
				reason = "harness exited: " + waitErr.Error()
			}
			if item.Order.Stage == core.StageReview {
				reason = "harness exited without terminal verdict submission"
			}
			if releaseErr := releaseAfterCheckpoint(core.WorkOrderOutcomeChildFailure, reason, exitStatus); releaseErr != nil {
				return fmt.Errorf("%s: release claim: %w", reason, releaseErr)
			}
			if waitErr != nil {
				return fmt.Errorf("%s: %w", reason, waitErr)
			}
			return errors.New(reason)
		}
		if renewed.State != core.WorkOrderSubmitted && renewed.State != core.WorkOrderCompleted {
			return fmt.Errorf("harness exited without authorized completion; server reports %s (%s)", renewed.State, reconciled.Reason)
		}
		// A successful review verdict submission is authoritative even when
		// the review child subsequently reports a non-zero exit.
		if waitErr != nil && item.Order.Stage != core.StageReview {
			return waitErr
		}
		return nil
	}
	for {
		select {
		case waitErr := <-done:
			return handleChildExit(waitErr)
		case <-runTerminalDeadline:
			// A normal child exit wins a race with the grace deadline.
			select {
			case waitErr := <-done:
				return handleChildExit(waitErr)
			default:
			}
			if checkpointReleaseReason != "" {
				if err := presentRunCheckpointPause(stdout, presentation, string(item.Order.Stage), checkpointReleaseReason); err != nil {
					return fmt.Errorf("present operator checkpoint pause: %w", err)
				}
			} else if err := presentRunChildReapNotice(stdout, presentation, string(item.Order.Stage), string(finalizedOrder.State)); err != nil {
				return fmt.Errorf("present lingering-child reap: %w", err)
			}
			_ = processGroup.terminate(nil)
			if processGroupAlive(processGroup.pgid) {
				return fmt.Errorf("lingering %s session process group %d survived termination", item.Order.Stage, processGroup.pgid)
			}
			flushOutput()
			reportCodexUsageFallback(c, credential, item.Order.ID, sessionID, finalizedOrder, codexUsage)
			return nil
		case <-firstActivityObserved:
			if !firstActivityTimer.Stop() {
				select {
				case <-firstActivityDeadline:
				default:
				}
			}
			firstActivityObserved = nil
			firstActivityDeadline = nil
		case <-activityObserved:
			if stallTimer != nil {
				drainActivity(activityObserved)
				stallGeneration, stallDue = resetStallTimerForActivity(stallTimer, firstActivity, stallTimeout)
				stallDeadline = stallTimer.C
			}
		case <-firstActivityDeadline:
			// Prefer output or normal child exit when either raced the timer.
			select {
			case <-firstActivity.observed:
				firstActivityObserved = nil
				firstActivityDeadline = nil
				continue
			default:
			}
			select {
			case waitErr := <-done:
				return handleChildExit(waitErr)
			default:
			}
			if claimFinalized {
				firstActivityObserved = nil
				firstActivityDeadline = nil
				continue
			}
			renewed, renewErr := renewDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt, activitySnapshot)
			if renewErr != nil {
				observedCheckpoint, checkpointErr := observeCheckpointRelease(renewErr)
				if observedCheckpoint {
					continue
				}
				_ = processGroup.terminate(nil)
				if checkpointErr != nil {
					return checkpointErr
				}
				if workerOrderPreempted(renewErr) {
					return attemptAuthorityLoss(errWorkerOrderPreempted.Error(), checkpointAttempt)
				}
				if workerOrderCancelled(renewErr) {
					return errWorkerOrderCancelled
				}
				_ = releaseAfterCheckpoint(core.WorkOrderOutcomeReleased, "claim authority lost: "+renewErr.Error(), nil)
				return renewErr
			}
			if renewed.State == core.WorkOrderSubmitted || renewed.State == core.WorkOrderCompleted {
				observeFinalized(renewed)
				continue
			}
			if renewed.State != core.WorkOrderClaimed {
				_ = processGroup.terminate(nil)
				return attemptAuthorityLoss("claim authority lost: server reports "+string(renewed.State), checkpointAttempt)
			}
			leaseExpiresAt = renewed.LeaseExpiresAt
			// The authority check may have overlapped the first byte or exit.
			select {
			case <-firstActivity.observed:
				firstActivityObserved = nil
				firstActivityDeadline = nil
				continue
			default:
			}
			select {
			case waitErr := <-done:
				return handleChildExit(waitErr)
			default:
			}
			waitErr := processGroup.terminate(nil)
			var exitStatus *int
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				status := exitErr.ExitCode()
				exitStatus = &status
			}
			if releaseErr := releaseAfterCheckpoint(core.WorkOrderOutcomeChildFailure, workerFirstActivityTimeoutReason, exitStatus); releaseErr != nil {
				return fmt.Errorf("%s: release claim: %w", workerFirstActivityTimeoutReason, releaseErr)
			}
			return errors.New(workerFirstActivityTimeoutReason)
		case firedAt := <-stallDeadline:
			// Output and normal completion win races at the inactivity boundary.
			if hook := workerStallDeadlineTestHook; hook != nil {
				hook()
			}
			drainActivity(activityObserved)
			generation, _ := firstActivity.snapshot()
			if generation != stallGeneration {
				stallGeneration, stallDue = resetStallTimerForActivity(stallTimer, firstActivity, stallTimeout)
				stallDeadline = stallTimer.C
				continue
			}
			if firedAt.Before(stallDue) {
				resetTimer(stallTimer, durationUntil(stallDue))
				stallDeadline = stallTimer.C
				continue
			}
			select {
			case waitErr := <-done:
				return handleChildExit(waitErr)
			default:
			}
			if claimFinalized {
				stallDeadline = nil
				continue
			}
			renewed, renewErr := renewDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt, activitySnapshot)
			if renewErr != nil {
				observedCheckpoint, checkpointErr := observeCheckpointRelease(renewErr)
				if observedCheckpoint {
					continue
				}
				_ = processGroup.terminate(nil)
				if checkpointErr != nil {
					return checkpointErr
				}
				if workerOrderPreempted(renewErr) {
					return attemptAuthorityLoss(errWorkerOrderPreempted.Error(), checkpointAttempt)
				}
				if workerOrderCancelled(renewErr) {
					return errWorkerOrderCancelled
				}
				_ = releaseAfterCheckpoint(core.WorkOrderOutcomeReleased, "claim authority lost: "+renewErr.Error(), nil)
				return renewErr
			}
			if renewed.State == core.WorkOrderSubmitted || renewed.State == core.WorkOrderCompleted {
				observeFinalized(renewed)
				continue
			}
			if renewed.State != core.WorkOrderClaimed {
				_ = processGroup.terminate(nil)
				return attemptAuthorityLoss("claim authority lost: server reports "+string(renewed.State), checkpointAttempt)
			}
			leaseExpiresAt = renewed.LeaseExpiresAt
			drainActivity(activityObserved)
			generation, _ = firstActivity.snapshot()
			if generation != stallGeneration {
				stallGeneration, stallDue = resetStallTimerForActivity(stallTimer, firstActivity, stallTimeout)
				stallDeadline = stallTimer.C
				continue
			}
			if time.Now().Before(stallDue) {
				resetTimer(stallTimer, durationUntil(stallDue))
				stallDeadline = stallTimer.C
				continue
			}
			select {
			case waitErr := <-done:
				return handleChildExit(waitErr)
			default:
			}
			if !firstActivity.generationUnchanged(stallGeneration) {
				stallGeneration, stallDue = resetStallTimerForActivity(stallTimer, firstActivity, stallTimeout)
				stallDeadline = stallTimer.C
				continue
			}
			waitErr := processGroup.terminate(nil)
			var exitStatus *int
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				status := exitErr.ExitCode()
				exitStatus = &status
			}
			if releaseErr := releaseAfterCheckpoint(core.WorkOrderOutcomeStalled, workerStallTimeoutReason, exitStatus); releaseErr != nil {
				return fmt.Errorf("%s: release claim: %w", workerStallTimeoutReason, releaseErr)
			}
			return errors.New(workerStallTimeoutReason)
		case <-ticker.C:
			if claimFinalized {
				continue
			}
			renewed, renewErr := renewDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt, activitySnapshot)
			if renewErr != nil {
				observedCheckpoint, checkpointErr := observeCheckpointRelease(renewErr)
				if observedCheckpoint {
					continue
				}
				_ = processGroup.terminate(nil)
				if checkpointErr != nil {
					return checkpointErr
				}
				if workerOrderPreempted(renewErr) {
					return attemptAuthorityLoss(errWorkerOrderPreempted.Error(), checkpointAttempt)
				}
				if workerOrderCancelled(renewErr) {
					return errWorkerOrderCancelled
				}
				_ = releaseAfterCheckpoint(core.WorkOrderOutcomeReleased, "claim authority lost: "+renewErr.Error(), nil)
				reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = c.reconcileDispatchOrderContext(reconcileCtx, credential, item, sessionID)
				cancel()
				return renewErr
			}
			if renewed.State == core.WorkOrderSubmitted || renewed.State == core.WorkOrderCompleted {
				observeFinalized(renewed)
				continue
			}
			if renewed.State != core.WorkOrderClaimed {
				_ = processGroup.terminate(nil)
				return attemptAuthorityLoss("claim authority lost: server reports "+string(renewed.State), checkpointAttempt)
			}
			leaseExpiresAt = renewed.LeaseExpiresAt
		case <-ctx.Done():
			_ = processGroup.terminate(nil)
			if item.Dispatch == "run" {
				// The explicit invocation owns renewal only for its own lifetime.
				// Leaving the claim for normal lease expiry makes a killed local run
				// claimable again without inventing a daemon or retry policy (AC-5.1).
				return ctx.Err()
			}
			_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			return ctx.Err()
		}
	}
}

func startHarnessCommandWithColdFallback(resume bool, argv, coldArgv []string, newCommand func([]string) *exec.Cmd) (*exec.Cmd, bool, error, error) {
	command := newCommand(argv)
	if err := command.Start(); err != nil {
		if !resume {
			return command, false, nil, err
		}
		resumeErr := err
		command = newCommand(coldArgv)
		if err = command.Start(); err != nil {
			return command, false, resumeErr, err
		}
		return command, false, resumeErr, nil
	}
	return command, resume, nil, nil
}

func attemptAuthorityLoss(reason string, checkpoint func(string) error) error {
	if checkpointErr := checkpoint(reason); checkpointErr != nil {
		return fmt.Errorf("%s; %w", reason, checkpointErr)
	}
	return errors.New(reason)
}

var workerProcessGroupTerminationGrace = 2 * time.Second

type harnessProcessGroup struct {
	pgid int
	done <-chan error
}

// terminate owns every post-start harness shutdown path. The harness is the
// leader of a dedicated process group, so TERM and the bounded KILL escalation
// cover dev servers, watchers, and other descendants as well as the child.
// A completed child may still have live descendants, so normal-exit cleanup
// also routes through this method (design-harness-execution).
func (g harnessProcessGroup) terminate(completed *error) error {
	if g.pgid <= 0 || g.pgid == syscall.Getpgrp() {
		if completed != nil {
			return *completed
		}
		return <-g.done
	}
	waitErr, waited := error(nil), false
	if completed != nil {
		waitErr, waited = *completed, true
	}
	_ = syscall.Kill(-g.pgid, syscall.SIGTERM)
	deadline := time.Now().Add(workerProcessGroupTerminationGrace)
	for processGroupAlive(g.pgid) && time.Now().Before(deadline) {
		if !waited {
			select {
			case waitErr = <-g.done:
				waited = true
			default:
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupAlive(g.pgid) {
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
	}
	if !waited {
		waitErr = <-g.done
	}
	killDeadline := time.Now().Add(workerProcessGroupTerminationGrace)
	for processGroupAlive(g.pgid) && time.Now().Before(killDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return waitErr
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

const workerFirstActivityTimeoutReason = "harness produced no output before first_activity_timeout"
const workerStallTimeoutReason = "harness produced no output before stall_timeout"

type firstActivitySignal struct {
	once       sync.Once
	observed   chan struct{}
	activity   chan struct{}
	mu         sync.Mutex
	last       time.Time
	generation uint64
}

func newFirstActivitySignal() *firstActivitySignal {
	return &firstActivitySignal{observed: make(chan struct{}), activity: make(chan struct{}, 1)}
}

func (s *firstActivitySignal) observe() {
	now := time.Now()
	s.mu.Lock()
	s.last = now
	s.generation++
	s.mu.Unlock()
	s.once.Do(func() { close(s.observed) })
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *firstActivitySignal) snapshot() (uint64, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation, s.last
}

func (s *firstActivitySignal) generationUnchanged(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation == generation
}

type firstActivityWriter struct {
	Destination io.Writer
	Signal      *firstActivitySignal
}

func (w *firstActivityWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.Signal.observe()
	}
	return w.Destination.Write(p)
}

func resetStallTimerForActivity(timer *time.Timer, signal *firstActivitySignal, timeout time.Duration) (uint64, time.Time) {
	generation, observedAt := signal.snapshot()
	due := observedAt.Add(timeout)
	now := time.Now()
	// A stale timer generation may be handled after the replacement deadline
	// has technically elapsed. Give the newly observed generation one complete
	// supervision window so the stale timer cannot classify it as stalled.
	if observedAt.IsZero() || !due.After(now) {
		due = now.Add(timeout)
	}
	resetTimer(timer, durationUntil(due))
	return generation, due
}

func durationUntil(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Nanosecond
	}
	return remaining
}

func drainActivity(activity <-chan struct{}) {
	for {
		select {
		case <-activity:
		default:
			return
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

type boundedTailWriter struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

const (
	// These worker-local limits match the shared observability wire contract.
	// The server independently bounds both inputs (req-260820-221be8 AC-1.2,
	// AC-2.2; design-260805-973cd4).
	workerActivitySnapshotLimit  = 4 * 1024
	workerAttemptTranscriptLimit = 4 * 1024 * 1024
)

func (w *boundedTailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data = append(w.data, p...)
	if len(w.data) > w.limit {
		w.data = append([]byte(nil), w.data[len(w.data)-w.limit:]...)
	}
	return len(p), nil
}

func (w *boundedTailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(w.data), "�"))
}

// boundedTranscriptSpool retains the newest redacted child output on disk.
// It is observability only and is never exposed to continuation or resume
// machinery (req-260820-221be8 AC-2.2, AC-3.3).
type boundedTranscriptSpool struct {
	mu         sync.Mutex
	file       *os.File
	path       string
	limit      int
	size       int
	truncated  bool
	captureErr error
}

func newBoundedTranscriptSpool(directory string, limit int) (*boundedTranscriptSpool, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("transcript spool limit must be positive")
	}
	file, err := os.CreateTemp(directory, ".attempt-transcript-*")
	if err != nil {
		return nil, err
	}
	return &boundedTranscriptSpool{file: file, path: file.Name(), limit: limit}, nil
}

func (s *boundedTranscriptSpool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return len(p), nil
	}
	written := len(p)
	if written == 0 {
		return 0, nil
	}
	if s.captureErr != nil {
		return written, nil
	}
	if written >= s.limit {
		if err := s.replaceLocked(p[written-s.limit:]); err != nil {
			s.captureErr = err
			return written, nil
		}
		s.truncated = true
		return written, nil
	}
	if s.size+written <= s.limit {
		n, err := s.file.Write(p)
		s.size += n
		if err != nil {
			s.captureErr = err
			return written, nil
		}
		if n != written {
			s.captureErr = io.ErrShortWrite
			return written, nil
		}
		return written, nil
	}
	keep := s.limit - written
	existing := make([]byte, keep)
	if _, err := s.file.ReadAt(existing, int64(s.size-keep)); err != nil {
		s.captureErr = err
		return written, nil
	}
	combined := append(existing, p...)
	if err := s.replaceLocked(combined); err != nil {
		s.captureErr = err
		return written, nil
	}
	s.truncated = true
	return written, nil
}

func (s *boundedTranscriptSpool) replaceLocked(content []byte) error {
	if err := s.file.Truncate(0); err != nil {
		return err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := s.file.Write(content)
	s.size = n
	if err != nil {
		return err
	}
	if n != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *boundedTranscriptSpool) Snapshot() (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return "", s.truncated, os.ErrClosed
	}
	if s.captureErr != nil {
		return "", s.truncated, s.captureErr
	}
	content := make([]byte, s.size)
	if s.size > 0 {
		if _, err := s.file.ReadAt(content, 0); err != nil {
			return "", s.truncated, err
		}
	}
	return strings.ToValidUTF8(string(content), "�"), s.truncated, nil
}

func (s *boundedTranscriptSpool) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	closeErr := s.file.Close()
	s.file = nil
	removeErr := os.Remove(s.path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

// workerClaimRenewInterval paces claim renewal for both pre-start setup and
// the running-child loop; tests shorten it to exercise renewal quickly.
var workerClaimRenewInterval = 10 * time.Second

// workerRunTerminalChildGrace gives a stage session time to exit on its own
// after the attached run observes durable submission or completion. Tests
// shorten it to exercise the launcher-side reap without slowing the suite.
var workerRunTerminalChildGrace = 2 * time.Second

// workerPreStartTestHook, when set by tests, runs with the setup context
// between a successful claim and child launch to simulate slow pre-start
// setup.
var workerPreStartTestHook func(context.Context)

// workerStallDeadlineTestHook lets tests hold a selected stall-timer
// generation while child output races that boundary.
var workerStallDeadlineTestHook func()

// preStartClaimRenewal keeps a claimed work order's lease renewed between a
// successful claim and child launch, when pre-start setup can outlast the
// lease window (design-260805-973cd4: renewal keeps the claim alive but never extends
// the fixed execution window). On authority loss it cancels the setup
// context so long-running setup steps abort instead of continuing unclaimed.
type preStartClaimRenewal struct {
	stop   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once

	mu    sync.Mutex
	lease time.Time
	lost  error
}

func startPreStartClaimRenewal(ctx context.Context, cancelSetup context.CancelFunc, c *client, credential string, item workerservice.DispatchOrder, sessionID string, lease time.Time, snapshot func() *core.WorkOrderActivitySnapshotInput) *preStartClaimRenewal {
	renewal := &preStartClaimRenewal{stop: make(chan struct{}), done: make(chan struct{}), cancel: cancelSetup, lease: lease}
	go func() {
		defer close(renewal.done)
		ticker := time.NewTicker(workerClaimRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewal.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewal.mu.Lock()
				current := renewal.lease
				renewal.mu.Unlock()
				renewed, err := renewDispatchClaimUntil(ctx, c, credential, item, sessionID, current, snapshot)
				if err == nil && renewed.State != core.WorkOrderClaimed {
					err = fmt.Errorf("server reports %s", renewed.State)
				}
				if err != nil {
					// Shutdown or handoff cancellation is not authority loss.
					if ctx.Err() != nil {
						return
					}
					renewal.mu.Lock()
					renewal.lost = err
					renewal.mu.Unlock()
					cancelSetup()
					return
				}
				renewal.mu.Lock()
				renewal.lease = renewed.LeaseExpiresAt
				renewal.mu.Unlock()
			}
		}
	}()
	return renewal
}

// Stop ends pre-start renewal (idempotent), joins the renewal goroutine, and
// reports the latest lease expiry plus any authority loss observed during
// setup. It cancels the setup context first so an in-flight renewal attempt
// unblocks promptly; by the time Stop is called, setup is either finished or
// being abandoned.
func (r *preStartClaimRenewal) Stop() (time.Time, error) {
	r.once.Do(func() {
		close(r.stop)
		r.cancel()
		<-r.done
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease, r.lost
}

// materializeSpecCheckout gives a spec agent repository-grounded, immutable
// base-branch context without creating the task branch (design-git-delivery).
func materializeSpecCheckout(ctx context.Context, root string, item workerservice.DispatchOrder) (string, error) {
	repository := item.Repository
	if strings.TrimSpace(repository.Name) == "" || repository.Name != item.Task.Repo {
		return "", fmt.Errorf("resolve spec repository %q from worker dispatch", item.Task.Repo)
	}
	if strings.TrimSpace(repository.URL) == "" {
		return "", fmt.Errorf("spec repository %q has no clone URL", repository.Name)
	}
	base := strings.TrimSpace(item.Task.BaseBranch)
	if base == "" {
		base = strings.TrimSpace(repository.Base)
	}
	if base == "" {
		return "", fmt.Errorf("spec repository %q has no base branch", repository.Name)
	}

	checkout := filepath.Join(root, "repository")
	if _, err := gitOutput(ctx, root, "clone", "--depth=1", "--single-branch", "--branch", base, "--", repository.URL, checkout); err != nil {
		return "", fmt.Errorf("materialize spec repository %q at %s: %w", repository.Name, base, err)
	}
	if _, err := gitOutput(ctx, checkout, "checkout", "--detach", "HEAD"); err != nil {
		return "", fmt.Errorf("detach spec repository %q at %s: %w", repository.Name, base, err)
	}
	if _, err := gitOutput(ctx, checkout, "remote", "set-url", "--push", "origin", "disabled://conveyor-spec-read-only"); err != nil {
		return "", fmt.Errorf("disable pushes from spec repository %q: %w", repository.Name, err)
	}
	if err := makeCheckoutReadOnly(checkout); err != nil {
		return "", fmt.Errorf("make spec repository %q read-only: %w", repository.Name, err)
	}
	return checkout, nil
}

func makeCheckoutReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
}

func makeCheckoutWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()|0o200)
	})
}

var errWorkerClaimAuthorityLost = errors.New("worker claim authority cannot be confirmed before lease safety margin")
var errWorkerOrderCancelled = errors.New("work order was cancelled by an operator")
var errWorkerOrderPreempted = errors.New("work order was preempted by an operator")

func workerOrderReleasedAtCheckpoint(err error) bool {
	var response *workerHTTPError
	return errors.As(err, &response) && response.StatusCode == http.StatusConflict &&
		(response.Code == "work_order_released_checkpoint" || strings.Contains(strings.ToLower(response.Message), "work_order_released_checkpoint"))
}

func workerOrderConflict(err error) bool {
	var response *workerHTTPError
	return errors.As(err, &response) && response.StatusCode == http.StatusConflict
}

func workerOrderCancelled(err error) bool {
	var response *workerHTTPError
	return errors.As(err, &response) && response.StatusCode == http.StatusConflict && strings.Contains(strings.ToLower(response.Message), "work order was cancelled")
}

func workerOrderPreempted(err error) bool {
	var response *workerHTTPError
	return errors.As(err, &response) && response.StatusCode == http.StatusConflict &&
		(response.Code == "work_order_preempted" || strings.Contains(strings.ToLower(response.Message), "work order was preempted"))
}

func reconcileWorkerClaimUntil(ctx context.Context, c *client, credential, orderID, sessionID string, leaseExpiresAt time.Time) (workerservice.ClaimReconciliation, error) {
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: orderID}, Dispatch: "worker"}
	return reconcileDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt)
}

func reconcileDispatchClaimUntil(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, sessionID string, leaseExpiresAt time.Time) (workerservice.ClaimReconciliation, error) {
	safetyDeadline := leaseExpiresAt.Add(-2 * time.Second)
	delay := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return workerservice.ClaimReconciliation{}, ctx.Err()
		}
		requestDeadline := time.Now().Add(5 * time.Second)
		if safetyDeadline.After(time.Now()) && safetyDeadline.Before(requestDeadline) {
			requestDeadline = safetyDeadline
		}
		requestCtx, cancel := context.WithDeadline(ctx, requestDeadline)
		result, err := c.reconcileDispatchOrderContext(requestCtx, credential, item, sessionID)
		cancel()
		if err == nil {
			return result, nil
		}
		if !transientWorkerError(err) {
			return workerservice.ClaimReconciliation{}, err
		}
		if !time.Now().Before(safetyDeadline) {
			return workerservice.ClaimReconciliation{}, errWorkerClaimAuthorityLost
		}
		wait := delay
		if remaining := time.Until(safetyDeadline); wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerservice.ClaimReconciliation{}, ctx.Err()
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func renewWorkerClaimUntil(ctx context.Context, c *client, credential, orderID, sessionID string, leaseExpiresAt time.Time) (core.WorkOrder, error) {
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: orderID}, Dispatch: "worker"}
	return renewDispatchClaimUntil(ctx, c, credential, item, sessionID, leaseExpiresAt, nil)
}

func renewDispatchClaimUntil(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, sessionID string, leaseExpiresAt time.Time, snapshot func() *core.WorkOrderActivitySnapshotInput) (core.WorkOrder, error) {
	if leaseExpiresAt.IsZero() {
		return core.WorkOrder{}, errWorkerClaimAuthorityLost
	}
	safetyMargin := 2 * time.Second
	remaining := time.Until(leaseExpiresAt)
	if smaller := remaining / 5; smaller > 0 && smaller < safetyMargin {
		safetyMargin = smaller
	}
	safetyDeadline := leaseExpiresAt.Add(-safetyMargin)
	delay := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return core.WorkOrder{}, ctx.Err()
		}
		if !time.Now().Before(safetyDeadline) {
			return core.WorkOrder{}, errWorkerClaimAuthorityLost
		}
		var activity *core.WorkOrderActivitySnapshotInput
		if snapshot != nil {
			activity = snapshot()
		}
		var renewed core.WorkOrder
		var err error
		if activity != nil {
			// Reserve most of the lease-safety window for the authoritative
			// renewal fallback when the advisory payload stalls in transport.
			advisoryBudget := time.Until(safetyDeadline) / 2
			if advisoryBudget > 2*time.Second {
				advisoryBudget = 2 * time.Second
			}
			advisoryCtx, cancelAdvisory := context.WithTimeout(ctx, advisoryBudget)
			renewed, err = c.renewDispatchOrderContext(advisoryCtx, credential, item, sessionID, activity)
			cancelAdvisory()
		}
		if activity == nil || err != nil {
			// AC-1.3: observability is advisory. Older servers and servers that
			// reject an optional snapshot still get the unchanged renewal request.
			requestCtx, cancel := context.WithDeadline(ctx, safetyDeadline)
			renewed, err = c.renewDispatchOrderContext(requestCtx, credential, item, sessionID, nil)
			cancel()
		}
		if err == nil {
			return renewed, nil
		}
		if !transientWorkerError(err) {
			return core.WorkOrder{}, fmt.Errorf("renew worker claim: %w", err)
		}
		remaining = time.Until(safetyDeadline)
		if remaining <= 0 {
			return core.WorkOrder{}, errWorkerClaimAuthorityLost
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return core.WorkOrder{}, ctx.Err()
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

// prepareMCPConfig preserves the JSON-file transport for existing harnesses
// while keeping scoped credentials out of TOML override argv (design-harness-execution).
func prepareMCPConfig(directory, base, credential, transport string) (string, error) {
	endpoint := strings.TrimRight(base, "/") + "/mcp"
	switch transport {
	case "", config.MCPTransportJSONFile:
		configPath := filepath.Join(directory, "mcp.json")
		mcp := map[string]any{"mcpServers": map[string]any{"conveyor": map[string]any{"url": endpoint, "headers": map[string]string{"Authorization": "Bearer " + credential}}}}
		data, err := json.Marshal(mcp)
		if err != nil {
			return "", fmt.Errorf("marshal MCP config: %w", err)
		}
		if err = os.WriteFile(configPath, data, 0o600); err != nil {
			return "", fmt.Errorf("write MCP config: %w", err)
		}
		return configPath, nil
	case config.MCPTransportTOMLOverride:
		return "mcp_servers.conveyor={url=" + strconv.Quote(endpoint) + ", bearer_token_env_var=\"CONVEYOR_API_TOKEN\"}", nil
	case config.MCPTransportEnvironment:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported MCP transport %q", transport)
	}
}

func isolatedChildEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := values[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

const (
	gitAskPassModeEnv     = "CONVEYOR_GIT_ASKPASS_MODE"
	gitAskPassTokenEnv    = "CONVEYOR_GIT_ASKPASS_TOKEN"
	gitAskPassUsernameEnv = "CONVEYOR_GIT_ASKPASS_USERNAME"
)

// childGitCredentialEnvironment keeps the forge token in process memory and
// the child-only environment. Git invokes this same executable as askpass;
// neither the token nor a token-bearing URL enters argv or Git configuration
// (req-260821-830dbf REQ-6/AC-6.1).
func childGitCredentialEnvironment(token string) (map[string]string, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Conveyor executable for Git askpass: %w", err)
	}
	return map[string]string{
		"GIT_ASKPASS":         executable,
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "never",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "credential.helper",
		"GIT_CONFIG_VALUE_0":  "",
		gitAskPassModeEnv:     "1",
		gitAskPassTokenEnv:    token,
		gitAskPassUsernameEnv: "x-access-token",
	}, nil
}

func runGitAskPass(output io.Writer, prompt string) error {
	lowerPrompt := strings.ToLower(strings.TrimSpace(prompt))
	switch {
	case strings.HasPrefix(lowerPrompt, "username for "):
		username := os.Getenv(gitAskPassUsernameEnv)
		if username == "" {
			return fmt.Errorf("Git askpass username is unavailable")
		}
		_, err := fmt.Fprintln(output, username)
		return err
	case strings.HasPrefix(lowerPrompt, "password for "):
		token := os.Getenv(gitAskPassTokenEnv)
		if token == "" {
			return fmt.Errorf("Git askpass token is unavailable")
		}
		_, err := fmt.Fprintln(output, token)
		return err
	default:
		return fmt.Errorf("Git askpass received an unsupported prompt")
	}
}

func isGitAskPassPrompt(prompt string) bool {
	lowerPrompt := strings.ToLower(strings.TrimSpace(prompt))
	return strings.HasPrefix(lowerPrompt, "username for ") || strings.HasPrefix(lowerPrompt, "password for ")
}

func requireHTTPSRemote(remote string) error {
	parsed, err := url.Parse(strings.TrimSpace(remote))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("repository origin must use an HTTPS URL without embedded credentials; configure origin with an https:// remote")
	}
	return nil
}

func validateHTTPSOrigin(ctx context.Context, directory string) error {
	remote, err := gitOutput(ctx, directory, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("inspect repository origin before credentialed Git work: %w", err)
	}
	return requireHTTPSRemote(remote)
}

func workerLaunchPrompt(order core.WorkOrder, workspace, sessionID string) string {
	prompt := fmt.Sprintf("Work on Conveyor work order %s in workspace %s using session_id %s.", order.ID, workspace, sessionID)
	if order.Stage == core.StageReview {
		return prompt + " Use the Conveyor MCP server and call get_work_order with that exact session_id for the approved contract. Complete the standard review lifecycle by calling submit_review_verdict, waiting for its response, and observing that the tool call succeeded before exiting. Printing, returning, or describing verdict JSON is not completion, and a missing or failed tool response is not terminal success."
	}
	if order.Stage == core.StageSpec {
		return prompt + " Use the Conveyor MCP server and call get_work_order with that exact session_id. Inspect the launched read-only repository checkout and artifacts without making edits or git changes; do not run conveyor checkout for a spec order. Complete the plan lifecycle by calling submit_plan, observing that the tool call succeeded, reporting the result, and exiting."
	}
	if order.Stage != core.StageImplement {
		return fmt.Sprintf("%s Use the Conveyor MCP server, call get_work_order with that exact session_id for the approved contract, and complete the standard %s lifecycle.", prompt, order.Stage)
	}
	return prompt + " Use the Conveyor MCP server. First call get_work_order with that exact session_id for the approved contract. Immediately after get_work_order returns, announce a concise, plain-language summary of what the work order is about and what you will do next. Make this announcement before running checkout, inspecting files, or starting implementation. The announcement is informational: continue automatically without asking for confirmation, waiting for a user response, or pausing. Then complete the standard implement lifecycle, reporting and exiting after submit_for_review succeeds without polling await_review."
}

func expandHarness(harness config.Harness, model, effort, prompt, mcpConfig string) []string {
	return expandHarnessWithEffortArgv(harness, model, harness.EffortArgs[effort], prompt, mcpConfig)
}

func expandHarnessWithEffortArgv(harness config.Harness, model string, effortArgv []string, prompt, mcpConfig string) []string {
	replace := func(values []string) []string {
		result := make([]string, len(values))
		for i, value := range values {
			switch value {
			case "{prompt}":
				result[i] = prompt
			case "{mcp_config}":
				result[i] = mcpConfig
			case "{model}":
				result[i] = model
			default:
				result[i] = value
			}
		}
		return result
	}
	argv := replace(harness.Command)
	if model != "" {
		argv = append(argv, replace(harness.ModelArgs)...)
	}
	if len(effortArgv) != 0 {
		argv = append(argv, effortArgv...)
	}
	return argv
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
