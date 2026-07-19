package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/spf13/cobra"
)

func workerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Enroll and run the operator-owned Auto dispatcher"}
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
	var once bool
	run := &cobra.Command{Use: "run", Short: "Heartbeat, claim Auto work, and supervise configured harnesses", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runWorker(ctx, newClient(), pairing, name, once)
	}}
	run.Flags().StringVar(&pairing, "pairing-token", "", "single-use token for first enrollment")
	run.Flags().StringVar(&name, "name", defaultWorkerName(), "worker display name")
	run.Flags().BoolVar(&once, "once", false, "process currently available work and exit")
	cmd.AddCommand(pair, list, revoke, run)
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
	return runWorkerWithPolicy(ctx, c, pairing, name, once, defaultWorkerReconnectPolicy)
}

func runWorkerWithPolicy(ctx context.Context, c *client, pairing, name string, once bool, reconnect workerReconnectPolicy) error {
	saved, err := loadOrEnrollWorker(c, pairing, name)
	if err != nil {
		return err
	}
	if c.workspace == "" {
		c.workspace = saved.Workspace
	}
	credential := saved.Credential
	active := map[string]core.Stage{}
	completions := make(chan childResult, 1024)
	started := false
	var mu sync.Mutex
	retryDelay := reconnect.Initial
	for {
		document, err := c.workerConfigContext(ctx, credential)
		if err != nil {
			if retryErr := waitForWorkerReconnect(ctx, reconnect, &retryDelay, "load worker configuration", err); retryErr != nil {
				return retryErr
			}
			continue
		}
		if err = validateWorkerConfig(document); err != nil {
			return fmt.Errorf("invalid worker configuration: %w", err)
		}
		probes := probeWorkerConfig(ctx, document)
		if _, err = c.heartbeatWorkerContext(ctx, credential, probes); err != nil {
			if retryErr := waitForWorkerReconnect(ctx, reconnect, &retryDelay, "heartbeat worker", err); retryErr != nil {
				return retryErr
			}
			continue
		}
		orders, err := c.listWorkerOrdersContext(ctx, credential)
		if err != nil {
			if retryErr := waitForWorkerReconnect(ctx, reconnect, &retryDelay, "poll work orders", err); retryErr != nil {
				return retryErr
			}
			continue
		}
		retryDelay = reconnect.Initial
		implementLimit := document.Execution.ImplementConcurrency
		if implementLimit < 1 {
			implementLimit = 1
		}
		reviewLimit := document.Execution.ReviewConcurrency
		if reviewLimit < 1 {
			reviewLimit = 1
		}
		counts := map[core.Stage]int{}
		mu.Lock()
		for _, stage := range active {
			counts[stage]++
		}
		mu.Unlock()
		for _, item := range orders {
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
			mu.Lock()
			active[item.Order.ID] = item.Order.Stage
			mu.Unlock()
			counts[item.Order.Stage]++
			started = true
			go func(order workerservice.DispatchOrder) {
				err := runHarnessChild(ctx, c, credential, order)
				completions <- childResult{stage: order.Order.Stage, err: err}
				mu.Lock()
				delete(active, order.Order.ID)
				mu.Unlock()
			}(item)
		}
		mu.Lock()
		activeCount := len(active)
		mu.Unlock()
		if once && activeCount == 0 && (started || len(orders) == 0) {
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
	return probeHarnessTargets(ctx, targets)
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

func runHarnessChild(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder) error {
	session, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate worker session: %w", err)
	}
	clientToken, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate worker client token: %w", err)
	}
	sessionID := "worker-" + session
	claimed, err := c.claimWorkerOrderContext(ctx, credential, item.Order.ID, sessionID, clientToken)
	if err != nil {
		return err
	}
	leaseExpiresAt := claimed.LeaseExpiresAt
	release := func(outcome, reason string, exitStatus *int) error {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.releaseWorkerOrderContext(releaseCtx, credential, item.Order.ID, core.WorkOrderRelease{SessionID: sessionID, Outcome: outcome, Reason: reason, ExitStatus: exitStatus})
	}
	directory, err := os.MkdirTemp("", "conveyor-worker-")
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "create worker temp directory failed", nil)
		return err
	}
	defer os.RemoveAll(directory)
	mcpConfig, err := prepareMCPConfig(directory, c.base, credential, item.Harness.MCPTransport)
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "prepare MCP config failed", nil)
		return err
	}
	prompt := workerLaunchPrompt(item.Order, c.workspace, sessionID)
	var effortArgv []string
	if item.Effort != "" {
		if item.Order.Stage == core.StageImplement {
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
	argv := expandHarnessWithEffortArgv(item.Harness, item.Model, effortArgv, prompt, mcpConfig)
	if len(argv) == 0 {
		_ = release(core.WorkOrderOutcomeReleased, "empty harness command", nil)
		return fmt.Errorf("harness %s has an empty command", item.Harness.Name)
	}
	childAddress := c.base
	if item.Harness.MCPTransport == config.MCPTransportEnvironment {
		childAddress = strings.TrimRight(c.base, "/") + "/mcp"
	}
	childEnv := isolatedChildEnvironment(os.Environ(), map[string]string{
		"CONVEYOR_API_TOKEN":     credential,
		"CONVEYOR_ADDR":          childAddress,
		"CONVEYOR_WORKSPACE":     c.workspace,
		"CONVEYOR_WORK_ORDER_ID": item.Order.ID,
		"CONVEYOR_SESSION_ID":    sessionID,
		"CONVEYOR_CLIENT_TOKEN":  clientToken,
	})
	workingDirectory, err := os.Getwd()
	if err != nil {
		_ = release(core.WorkOrderOutcomeReleased, "resolve worker directory failed", nil)
		return fmt.Errorf("resolve worker directory: %w", err)
	}
	if item.Harness.MCPTransport == config.MCPTransportEnvironment {
		if err = validateGrokEnvironmentAttachment(ctx, item.Harness, childEnv, workingDirectory); err != nil {
			_ = release(core.WorkOrderOutcomeReleased, "environment MCP readiness failed", nil)
			return err
		}
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.Env = childEnv
	if err = command.Start(); err != nil {
		if ctx.Err() != nil {
			_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			return ctx.Err()
		}
		_ = release(core.WorkOrderOutcomeChildFailure, "harness launch failed: "+err.Error(), nil)
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	claimFinalized := false
	for {
		select {
		case waitErr := <-done:
			if ctx.Err() != nil {
				_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
				return ctx.Err()
			}
			var exitStatus *int
			if waitErr != nil {
				var exitErr *exec.ExitError
				if errors.As(waitErr, &exitErr) {
					status := exitErr.ExitCode()
					exitStatus = &status
				}
			}
			// Preserve the generic implementation failure path. Review children
			// reconcile terminal state because a verdict can commit before a
			// harness reports a later non-zero exit.
			if item.Order.Stage != core.StageReview && waitErr != nil {
				_ = release(core.WorkOrderOutcomeChildFailure, "harness exited: "+waitErr.Error(), exitStatus)
				return waitErr
			}
			reconciled, reconcileErr := reconcileWorkerClaimUntil(ctx, c, credential, item.Order.ID, sessionID, leaseExpiresAt)
			if reconcileErr != nil {
				_ = release(core.WorkOrderOutcomeChildFailure, "could not confirm work-order completion", exitStatus)
				return fmt.Errorf("confirm work-order completion: %w", reconcileErr)
			}
			renewed := reconciled.WorkOrder
			if renewed.State == core.WorkOrderClaimed && reconciled.Authorized {
				reason := "harness exited before completing work order"
				if item.Order.Stage == core.StageReview {
					reason = "harness exited without terminal verdict submission"
				}
				if releaseErr := release(core.WorkOrderOutcomeChildFailure, reason, exitStatus); releaseErr != nil {
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
			return nil
		case <-ticker.C:
			if claimFinalized {
				continue
			}
			renewed, renewErr := renewWorkerClaimUntil(ctx, c, credential, item.Order.ID, sessionID, leaseExpiresAt)
			if renewErr != nil {
				_ = command.Process.Kill()
				<-done
				_ = release(core.WorkOrderOutcomeReleased, "claim authority lost: "+renewErr.Error(), nil)
				reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = c.reconcileWorkerOrderReadOnlyContext(reconcileCtx, credential, item.Order.ID, sessionID)
				cancel()
				return renewErr
			}
			if renewed.State == core.WorkOrderSubmitted || renewed.State == core.WorkOrderCompleted {
				claimFinalized = true
				continue
			}
			if renewed.State != core.WorkOrderClaimed {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("claim authority lost: server reports %s", renewed.State)
			}
			leaseExpiresAt = renewed.LeaseExpiresAt
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-done
			_ = release(core.WorkOrderOutcomeCancelled, "worker shutting down", nil)
			return ctx.Err()
		}
	}
}

var errWorkerClaimAuthorityLost = errors.New("worker claim authority cannot be confirmed before lease safety margin")

func reconcileWorkerClaimUntil(ctx context.Context, c *client, credential, orderID, sessionID string, leaseExpiresAt time.Time) (workerservice.ClaimReconciliation, error) {
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
		result, err := c.reconcileWorkerOrderContext(requestCtx, credential, orderID, sessionID)
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
		requestCtx, cancel := context.WithDeadline(ctx, safetyDeadline)
		renewed, err := c.renewWorkerOrderContext(requestCtx, credential, orderID, sessionID)
		cancel()
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
// while keeping scoped credentials out of TOML override argv (spec §21.20).
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
	for _, name := range []string{"CONVEYOR_API_TOKEN", "CONVEYOR_ADDR", "CONVEYOR_WORKSPACE", "CONVEYOR_WORK_ORDER_ID", "CONVEYOR_SESSION_ID", "CONVEYOR_CLIENT_TOKEN"} {
		result = append(result, name+"="+values[name])
	}
	return result
}

func workerLaunchPrompt(order core.WorkOrder, workspace, sessionID string) string {
	prompt := fmt.Sprintf("Work on Conveyor work order %s in workspace %s using session_id %s.", order.ID, workspace, sessionID)
	if order.Stage == core.StageReview {
		return prompt + " Use the Conveyor MCP server and call get_work_order with that exact session_id for the approved contract. Complete the standard review lifecycle by calling submit_review_verdict, waiting for its response, and observing that the tool call succeeded before exiting. Printing, returning, or describing verdict JSON is not completion, and a missing or failed tool response is not terminal success."
	}
	if order.Stage != core.StageImplement {
		return fmt.Sprintf("%s Use the Conveyor MCP server, call get_work_order with that exact session_id for the approved contract, and complete the standard %s lifecycle.", prompt, order.Stage)
	}
	return prompt + " Use the Conveyor MCP server. First call get_work_order with that exact session_id for the approved contract. Immediately after get_work_order returns, announce a concise, plain-language summary of what the work order is about and what you will do next. Make this announcement before running checkout, inspecting files, or starting implementation. The announcement is informational: continue automatically without asking for confirmation, waiting for a user response, or pausing. Then complete the standard implement lifecycle."
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
