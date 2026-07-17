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

func runWorker(ctx context.Context, c *client, pairing, name string, once bool) error {
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
	for {
		document, err := c.workerConfig(credential)
		if err != nil {
			return err
		}
		probes := probeWorkerConfig(ctx, document)
		if _, err = c.heartbeatWorker(credential, probes); err != nil {
			return err
		}
		orders, err := c.listWorkerOrders(credential)
		if err != nil {
			return err
		}
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
	if _, err := c.claimWorkerOrder(credential, item.Order.ID, sessionID, clientToken); err != nil {
		return err
	}
	release := func(reason string) {
		_ = c.releaseWorkerOrder(credential, item.Order.ID, reason)
	}
	directory, err := os.MkdirTemp("", "conveyor-worker-")
	if err != nil {
		release("create worker temp directory failed")
		return err
	}
	defer os.RemoveAll(directory)
	configPath := filepath.Join(directory, "mcp.json")
	mcp := map[string]any{"mcpServers": map[string]any{"conveyor": map[string]any{"url": strings.TrimRight(c.base, "/") + "/mcp", "headers": map[string]string{"Authorization": "Bearer " + credential}}}}
	data, _ := json.Marshal(mcp)
	if err = os.WriteFile(configPath, data, 0o600); err != nil {
		release("write MCP config failed")
		return err
	}
	prompt := workerLaunchPrompt(item.Order, c.workspace, sessionID)
	argv := expandHarness(item.Harness, item.Model, prompt, configPath)
	if len(argv) == 0 {
		release("empty harness command")
		return fmt.Errorf("harness %s has an empty command", item.Harness.Name)
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.Env = append(os.Environ(), "CONVEYOR_API_TOKEN="+credential, "CONVEYOR_ADDR="+c.base, "CONVEYOR_WORKSPACE="+c.workspace, "CONVEYOR_WORK_ORDER_ID="+item.Order.ID, "CONVEYOR_SESSION_ID="+sessionID)
	if err = command.Start(); err != nil {
		release("harness launch failed")
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-done:
			if waitErr != nil {
				release("harness exited: " + waitErr.Error())
				return waitErr
			}
			renewed, renewErr := c.renewWorkerOrder(credential, item.Order.ID)
			if renewErr != nil {
				release("could not confirm work-order completion")
				return fmt.Errorf("confirm work-order completion: %w", renewErr)
			}
			if renewed.State == core.WorkOrderClaimed {
				release("harness exited before completing work order")
				return errors.New("harness exited before completing work order")
			}
			if renewed.State != core.WorkOrderSubmitted && renewed.State != core.WorkOrderCompleted {
				return fmt.Errorf("harness exited with work order in unexpected state %s", renewed.State)
			}
			return nil
		case <-ticker.C:
			if _, renewErr := c.renewWorkerOrder(credential, item.Order.ID); renewErr != nil {
				_ = command.Process.Kill()
				<-done
				return renewErr
			}
		case <-ctx.Done():
			release("worker shutting down")
			_ = command.Process.Kill()
			<-done
			return ctx.Err()
		}
	}
}

func workerLaunchPrompt(order core.WorkOrder, workspace, sessionID string) string {
	prompt := fmt.Sprintf("Work on Conveyor work order %s in workspace %s using session_id %s.", order.ID, workspace, sessionID)
	if order.Stage != core.StageImplement {
		return fmt.Sprintf("%s Use the Conveyor MCP server, call get_work_order with that exact session_id for the approved contract, and complete the standard %s lifecycle.", prompt, order.Stage)
	}
	return prompt + " Use the Conveyor MCP server. First call get_work_order with that exact session_id for the approved contract. Immediately after get_work_order returns, announce a concise, plain-language summary of what the work order is about and what you will do next. Make this announcement before running checkout, inspecting files, or starting implementation. The announcement is informational: continue automatically without asking for confirmation, waiting for a user response, or pausing. Then complete the standard implement lifecycle."
}

func expandHarness(harness config.Harness, model, prompt, mcpConfig string) []string {
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
	return argv
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
