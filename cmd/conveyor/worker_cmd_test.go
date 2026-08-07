package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func runHarnessChild(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder) error {
	return runHarnessChildWithFirstActivityTimeout(ctx, c, credential, item, config.DefaultFirstActivityTimeout)
}

func runHarnessChildWithOutput(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, stdout, stderr io.Writer) error {
	return runHarnessChildWithFirstActivityTimeoutAndOutput(ctx, c, credential, item, config.DefaultFirstActivityTimeout, stdout, stderr)
}

func TestWorkerHarnessProbesRetryUnhealthyWithBoundedBackoffAndReportRecovery(t *testing.T) {
	now := time.Now().UTC()
	document := workerservice.WorkerConfig{WorkspaceDocument: config.WorkspaceDocument{Harnesses: []config.Harness{{Name: "codex", ProbeCommand: []string{"codex", "--version"}}}}}
	tracker := newWorkerHarnessProbes()
	calls := 0
	tracker.run = func(_ context.Context, targets []workerservice.HarnessProbeTarget) []core.HarnessProbe {
		calls++
		healthy := calls >= 3
		return []core.HarnessProbe{{Harness: targets[0].Harness.Name, Fingerprint: targets[0].Fingerprint, Healthy: healthy, CheckedAt: now}}
	}
	if probes := tracker.probe(t.Context(), document, now); len(probes) != 1 || probes[0].Healthy || calls != 1 {
		t.Fatalf("initial probes=%+v calls=%d", probes, calls)
	}
	if probes := tracker.probe(t.Context(), document, now.Add(workerProbeRetryInitial-time.Millisecond)); len(probes) != 1 || probes[0].Healthy || calls != 1 {
		t.Fatalf("early retry probes=%+v calls=%d", probes, calls)
	}
	if probes := tracker.probe(t.Context(), document, now.Add(workerProbeRetryInitial)); len(probes) != 1 || probes[0].Healthy || calls != 2 {
		t.Fatalf("first retry probes=%+v calls=%d", probes, calls)
	}
	recoveredAt := now.Add(workerProbeRetryInitial + 2*workerProbeRetryInitial)
	probes := tracker.probe(t.Context(), document, recoveredAt)
	if len(probes) != 1 || !probes[0].Healthy || probes[0].Transition != "unhealthy_to_healthy" || calls != 3 {
		t.Fatalf("recovery probes=%+v calls=%d", probes, calls)
	}
	tracker.acknowledgeTransitions()
	if probes = tracker.probe(t.Context(), document, recoveredAt.Add(time.Second)); len(probes) != 1 || probes[0].Transition != "" || calls != 3 {
		t.Fatalf("acknowledged probes=%+v calls=%d", probes, calls)
	}
}

func TestBoundedTailCapturesOnlyRedactedFinalTwoKiB(t *testing.T) {
	secret := "child-only-runtime-secret"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	tail := &boundedTailWriter{limit: workerservice.FailureDetailLimit}
	var console bytes.Buffer
	redactor := redact.New([]string{secret})
	stdout := &redact.Writer{Destination: io.MultiWriter(&console, tail), Redactor: redactor}
	stderr := &redact.Writer{Destination: io.MultiWriter(&console, tail), Redactor: redactor}
	_, _ = stdout.Write([]byte(strings.Repeat("prefix", 500) + "\n"))
	_, _ = stderr.Write([]byte("raw=" + secret + " encoded=" + encoded + "\nfinal provider error\n"))
	_ = stdout.Flush()
	_ = stderr.Flush()
	detail := tail.String()
	if len(detail) > workerservice.FailureDetailLimit || strings.Contains(detail, secret) || strings.Contains(detail, encoded) {
		t.Fatalf("unsafe bounded detail len=%d detail=%q", len(detail), detail)
	}
	if !strings.Contains(detail, "[REDACTED:exact]") || !strings.Contains(detail, "[REDACTED:encoded]") || !strings.HasSuffix(detail, "final provider error") {
		t.Fatalf("captured detail=%q", detail)
	}
}

func TestExpandHarnessUsesWholeElementSubstitutionAndOptionalModelArgs(t *testing.T) {
	harness := config.Harness{MCPTransport: config.MCPTransportTOMLOverride, Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}}
	override := `mcp_servers.conveyor={url="http://127.0.0.1:8080/mcp", bearer_token_env_var="CONVEYOR_API_TOKEN"}`
	want := []string{"codex", "exec", "do work", "--config", override, "--model", "gpt"}
	if got := expandHarness(harness, "gpt", "", "do work", override); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%q want=%q", got, want)
	}
	want = want[:5]
	if got := expandHarness(harness, "", "", "do work", override); !reflect.DeepEqual(got, want) {
		t.Fatalf("without model argv=%q want=%q", got, want)
	}
}

func TestCodexUsageCollectorAcceptsOnlyValidTurnCompletedJSONL(t *testing.T) {
	collector := &codexUsageCollector{}
	_, _ = collector.Write([]byte(`{"type":"item.completed","usage":{"input_tokens":1,"output_tokens":2}}` + "\n"))
	_, _ = collector.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":21262,`))
	_, _ = collector.Write([]byte(`"output_tokens":5}}` + "\n" + `not-json` + "\n"))

	usage, ok := collector.Usage()
	if !ok || usage != (codexUsageTotals{TokensIn: 21262, TokensOut: 5}) {
		t.Fatalf("usage=%+v available=%v", usage, ok)
	}
	_, _ = collector.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":-1,"output_tokens":7}}` + "\n"))
	if got, _ := collector.Usage(); got != usage {
		t.Fatalf("invalid terminal payload replaced usage: %+v", got)
	}
}

func TestEnableCodexJSONOutputIsNarrowAndIdempotent(t *testing.T) {
	original := []string{"codex", "exec", "prompt", "config"}
	got, collector := enableCodexJSONOutput(config.Harness{Name: "codex"}, original)
	want := []string{"codex", "exec", "--json", "prompt", "config"}
	if collector == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("codex argv=%q collector=%v", got, collector != nil)
	}
	got, collector = enableCodexJSONOutput(config.Harness{Name: "codex"}, got)
	if collector == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("idempotent argv=%q collector=%v", got, collector != nil)
	}
	got, collector = enableCodexJSONOutput(config.Harness{Name: "claude"}, original)
	if collector != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("non-codex argv=%q collector=%v", got, collector != nil)
	}
}

func TestReportCodexUsageFallbackIsBestEffortReplacementOnly(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var arguments map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer worker-token" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		mu.Lock()
		calls++
		arguments = request.Params.Arguments
		mu.Unlock()
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo"}

	valid := &codexUsageCollector{}
	_, _ = valid.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":21,"output_tokens":3}}` + "\n"))
	reportCodexUsageFallback(c, "worker-token", "order-1", "session-1", core.WorkOrder{}, valid)

	invalid := &codexUsageCollector{}
	_, _ = invalid.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":"bad","output_tokens":3}}` + "\n"))
	reportCodexUsageFallback(c, "worker-token", "order-1", "session-1", core.WorkOrder{}, invalid)
	reportCodexUsageFallback(c, "worker-token", "order-1", "session-1", core.WorkOrder{}, &codexUsageCollector{})
	reportCodexUsageFallback(c, "worker-token", "order-1", "session-1", core.WorkOrder{UsageReported: true}, valid)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("fallback calls=%d want=1", calls)
	}
	if arguments["source"] != "worker_fallback" || arguments["workspace_id"] != "demo" || arguments["tokens_in"] != float64(21) || arguments["tokens_out"] != float64(3) {
		t.Fatalf("fallback arguments=%v", arguments)
	}
}

func TestPrepareMCPConfigPreservesJSONFileSecurityAndBuildsSecretFreeTOML(t *testing.T) {
	directory := t.TempDir()
	jsonPath, err := prepareMCPConfig(directory, "http://127.0.0.1:8080/", "worker-secret", config.MCPTransportJSONFile)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("MCP config permissions=%o want=600", info.Mode().Perm())
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("Bearer worker-secret")) {
		t.Fatalf("JSON transport did not write the scoped credential: %s", data)
	}

	override, err := prepareMCPConfig(directory, "http://127.0.0.1:8080/", "worker-secret", config.MCPTransportTOMLOverride)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(override, "worker-secret") || !strings.Contains(override, `bearer_token_env_var="CONVEYOR_API_TOKEN"`) || !strings.Contains(override, `url="http://127.0.0.1:8080/mcp"`) {
		t.Fatalf("unsafe or invalid TOML override: %s", override)
	}

	environment, err := prepareMCPConfig(directory, "http://127.0.0.1:8080/", "worker-secret", config.MCPTransportEnvironment)
	if err != nil || environment != "" {
		t.Fatalf("environment transport generated config %q: %v", environment, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("environment transport wrote a generated file: entries=%d err=%v", len(entries), err)
	}
}

func TestIsolatedChildEnvironmentReplacesLaunchIdentity(t *testing.T) {
	env := isolatedChildEnvironment([]string{"PATH=/bin", "CONVEYOR_API_TOKEN=stale", "CONVEYOR_SESSION_ID=stale", "CONVEYOR_TASK_ID=stale-task"}, map[string]string{
		"CONVEYOR_API_TOKEN": "fresh", "CONVEYOR_ADDR": "endpoint", "CONVEYOR_WORKSPACE": "demo",
		"CONVEYOR_WORK_ORDER_ID": "order", "CONVEYOR_SESSION_ID": "session", "CONVEYOR_CLIENT_TOKEN": "client",
		"CONVEYOR_TASK_ID": "task-1", "CONVEYOR_TASK_BRANCH": "conveyor/task-1",
		"CONVEYOR_TASK_BASE_BRANCH": "main", "CONVEYOR_TASK_REPO": "conveyor",
		"CONVEYOR_TASK_REPO_URL": "https://github.com/kidus-tiliksew/conveyor.git",
	})
	for name, want := range map[string]string{
		"CONVEYOR_API_TOKEN": "fresh", "CONVEYOR_ADDR": "endpoint", "CONVEYOR_WORKSPACE": "demo",
		"CONVEYOR_WORK_ORDER_ID": "order", "CONVEYOR_SESSION_ID": "session", "CONVEYOR_CLIENT_TOKEN": "client",
		"CONVEYOR_TASK_ID": "task-1", "CONVEYOR_TASK_BRANCH": "conveyor/task-1",
		"CONVEYOR_TASK_BASE_BRANCH": "main", "CONVEYOR_TASK_REPO": "conveyor",
		"CONVEYOR_TASK_REPO_URL": "https://github.com/kidus-tiliksew/conveyor.git",
	} {
		if got := environmentValue(env, name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}
	other := isolatedChildEnvironment(env, map[string]string{
		"CONVEYOR_API_TOKEN": "other-token", "CONVEYOR_ADDR": "other-endpoint", "CONVEYOR_WORKSPACE": "demo",
		"CONVEYOR_WORK_ORDER_ID": "other-order", "CONVEYOR_SESSION_ID": "other-session", "CONVEYOR_CLIENT_TOKEN": "other-client",
	})
	if environmentValue(env, "CONVEYOR_SESSION_ID") != "session" || environmentValue(other, "CONVEYOR_SESSION_ID") != "other-session" || environmentValue(other, "CONVEYOR_CLIENT_TOKEN") != "other-client" {
		t.Fatalf("concurrent child environments shared launch identity")
	}
}

func TestCodexParsesGeneratedMCPOverride(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	override, err := prepareMCPConfig(t.TempDir(), "http://127.0.0.1:8080", "must-not-appear", config.MCPTransportTOMLOverride)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(codex, "mcp", "get", "conveyor", "--config", override)
	command.Env = append(os.Environ(), "CODEX_HOME="+t.TempDir(), "CONVEYOR_API_TOKEN=parser-test")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex rejected generated MCP override: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("bearer_token_env_var: CONVEYOR_API_TOKEN")) {
		t.Fatalf("Codex parsed an unexpected MCP definition:\n%s", output)
	}
}

func TestExpandHarnessAppendsExplicitAdapterEffortArgs(t *testing.T) {
	codex := config.Harness{Command: []string{"codex", "{prompt}", "{mcp_config}"}, EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}}
	claude := config.Harness{Command: []string{"claude", "{prompt}", "{mcp_config}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}}
	if got, want := expandHarness(codex, "", "high", "review", "/tmp/mcp.json"), []string{"codex", "review", "/tmp/mcp.json", "--config", `model_reasoning_effort="high"`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("codex argv=%q want=%q", got, want)
	}
	if got, want := expandHarness(claude, "", "high", "review", "/tmp/mcp.json"), []string{"claude", "review", "/tmp/mcp.json", "--effort", "high"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude argv=%q want=%q", got, want)
	}
	if got := expandHarness(codex, "", "", "review", "/tmp/mcp.json"); len(got) != 3 {
		t.Fatalf("legacy seat received effort override: %q", got)
	}
}

func TestImplementationExpansionUsesCapturedEffortArgvAfterHotReload(t *testing.T) {
	harness := config.Harness{
		Command:    []string{"codex", "{prompt}", "{mcp_config}"},
		EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="low"`}},
	}
	captured := []string{"--config", `model_reasoning_effort="high"`}
	got := expandHarnessWithEffortArgv(harness, "", captured, "implement", "/tmp/mcp.json")
	want := []string{"codex", "implement", "/tmp/mcp.json", "--config", `model_reasoning_effort="high"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured implementation argv=%q want=%q", got, want)
	}
	if got := expandHarnessWithEffortArgv(harness, "", nil, "implement", "/tmp/mcp.json"); len(got) != 3 {
		t.Fatalf("unset implementation effort appended argv: %q", got)
	}
}

func TestWorkerLaunchPromptRequiresNonBlockingImplementationAnnouncement(t *testing.T) {
	prompt := workerLaunchPrompt(core.WorkOrder{ID: "implement-order", Stage: core.StageImplement}, "demo", "worker-session")
	requiredInOrder := []string{
		"First call get_work_order",
		"Immediately after get_work_order returns",
		"plain-language summary of what the work order is about and what you will do next",
		"before running checkout, inspecting files, or starting implementation",
		"continue automatically without asking for confirmation, waiting for a user response, or pausing",
	}
	position := -1
	for _, required := range requiredInOrder {
		next := strings.Index(prompt, required)
		if next < 0 {
			t.Fatalf("implementation prompt is missing %q: %s", required, prompt)
		}
		if next <= position {
			t.Fatalf("implementation prompt places %q out of order: %s", required, prompt)
		}
		position = next
	}
}

func TestWorkerLaunchPromptDoesNotGiveReviewOrdersImplementationInstructions(t *testing.T) {
	prompt := workerLaunchPrompt(core.WorkOrder{ID: "review-order", Stage: core.StageReview}, "demo", "worker-session")
	if strings.Contains(prompt, "plain-language summary") || strings.Contains(prompt, "running checkout") {
		t.Fatalf("review prompt contains implementation-only announcement instructions: %s", prompt)
	}
	for _, required := range []string{
		"call get_work_order with that exact session_id",
		"standard review lifecycle",
		"calling submit_review_verdict",
		"waiting for its response",
		"observing that the tool call succeeded before exiting",
		"Printing, returning, or describing verdict JSON is not completion",
		"a missing or failed tool response is not terminal success",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("review prompt is missing %q: %s", required, prompt)
		}
	}
}

func TestRunWorkerReconnectsAcrossRetryableControlPlaneFailures(t *testing.T) {
	t.Setenv("CONVEYOR_WORKER_TOKEN", "saved-enrollment-credential")
	var mu sync.Mutex
	counts := map[string]int{}
	document := workerservice.WorkerConfig{WorkspaceDocument: config.WorkspaceDocument{
		Workspace: "demo",
		Harnesses: []config.Harness{{Name: "codex", Command: []string{"true"}, ProbeCommand: []string{"true"}, ProbeTimeoutText: "1s"}},
		Execution: config.ExecutionPolicy{ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		attempt := counts[r.URL.Path]
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer saved-enrollment-credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if attempt == 1 {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v1/worker/config":
			_ = json.NewEncoder(w).Encode(document)
		case "/v1/worker/heartbeat":
			_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker-1"})
		case "/v1/worker/work-orders":
			_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	policy := workerReconnectPolicy{Initial: time.Millisecond, Maximum: 4 * time.Millisecond, Jitter: func(delay time.Duration) time.Duration { return delay }}
	if err := runWorkerWithPolicy(t.Context(), &client{base: server.URL, workspace: "demo"}, "", "test", true, policy); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/worker/config", "/v1/worker/heartbeat", "/v1/worker/work-orders"} {
		if counts[path] < 2 {
			t.Fatalf("%s attempts=%d want reconnect", path, counts[path])
		}
	}
}

func TestRunWorkerTreatsRevokedCredentialAsTerminalAndCancellationInterruptsBackoff(t *testing.T) {
	t.Setenv("CONVEYOR_WORKER_TOKEN", "revoked")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	policy := workerReconnectPolicy{Initial: time.Second, Maximum: time.Second, Jitter: func(delay time.Duration) time.Duration { return delay }}
	err := runWorkerWithPolicy(t.Context(), &client{base: server.URL, workspace: "demo"}, "", "test", true, policy)
	server.Close()
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") || requests != 1 {
		t.Fatalf("terminal revoked credential err=%v requests=%d", err, requests)
	}

	retrying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "restarting", http.StatusServiceUnavailable)
	}))
	defer retrying.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runWorkerWithPolicy(ctx, &client{base: retrying.URL, workspace: "demo"}, "", "test", true, policy)
	}()
	time.AfterFunc(20*time.Millisecond, cancel)
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation err=%v", err)
	}
}

func TestRunWorkerShutdownWaitsForActiveChildCleanup(t *testing.T) {
	t.Setenv("CONVEYOR_WORKER_TOKEN", "worker-credential")
	pidFile := filepath.Join(t.TempDir(), "harness.pid")
	t.Setenv("CONVEYOR_FAKE_HARNESS_PID_FILE", pidFile)
	claimed := make(chan struct{}, 1)
	released := make(chan core.WorkOrderRelease, 1)
	completeRelease := make(chan struct{})
	var completeReleaseOnce sync.Once
	finishRelease := func() { completeReleaseOnce.Do(func() { close(completeRelease) }) }
	defer finishRelease()
	document := workerservice.WorkerConfig{WorkspaceDocument: config.WorkspaceDocument{
		Workspace: "demo",
		Harnesses: []config.Harness{{
			Name: "helper", Command: []string{"true"}, ProbeCommand: []string{"true"}, ProbeTimeoutText: "1s",
		}},
		Execution: config.ExecutionPolicy{ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
	}}
	item := workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "shutdown-active-child", Stage: core.StageImplement},
		Harness: config.Harness{
			Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "cancel"},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/worker/config":
			_ = json.NewEncoder(w).Encode(document)
		case "/v1/worker/heartbeat":
			_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker-1"})
		case "/v1/worker/work-orders":
			_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{item})
		case "/v1/worker/work-orders/shutdown-active-child/claim":
			claimed <- struct{}{}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{
				ID: item.Order.ID, State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Minute),
			})
		case "/v1/worker/work-orders/shutdown-active-child/release":
			var release core.WorkOrderRelease
			if err := json.NewDecoder(r.Body).Decode(&release); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			released <- release
			<-completeRelease
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: item.Order.ID, State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runWorkerWithPolicy(ctx, &client{base: server.URL, workspace: "demo"}, "", "test", false, defaultWorkerReconnectPolicy)
	}()
	<-claimed
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
		}
		if pid == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatal("harness child did not start")
	}
	cancel()
	select {
	case release := <-released:
		if release.Outcome != core.WorkOrderOutcomeCancelled || release.Reason != "worker shutting down" {
			t.Fatalf("release=%+v", release)
		}
	case err := <-done:
		t.Fatalf("worker returned before releasing active child claim: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("active child claim was not released")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("harness child %d is still running after worker shutdown", pid)
	}
	select {
	case err = <-done:
		t.Fatalf("worker returned before claim release completed: %v", err)
	default:
	}
	finishRelease()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("worker shutdown error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not return after child cleanup completed")
	}
}

func TestRunHarnessChildStopsWithoutRetryWhenOrderIsCancelled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "cancelled.pid")
	t.Setenv("CONVEYOR_FAKE_HARNESS_PID_FILE", pidFile)
	renewed := make(chan struct{}, 1)
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "cancel-active-child", State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(300 * time.Millisecond)})
		case strings.HasSuffix(r.URL.Path, "/renew"):
			select {
			case renewed <- struct{}{}:
			default:
			}
			http.Error(w, store.ErrWorkOrderCancelled.Error(), http.StatusConflict)
		case strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "cancel-active-child", State: core.WorkOrderCancelled})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "cancel-active-child", Stage: core.StageImplement}, Harness: config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "cancel"}}}
	err := runHarnessChildWithOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "credential", item, io.Discard, io.Discard)
	if !errors.Is(err, errWorkerOrderCancelled) {
		t.Fatalf("run error=%v", err)
	}
	select {
	case <-renewed:
	default:
		t.Fatal("worker never observed the cancelled order")
	}
	if releases != 0 {
		t.Fatalf("cancelled child released for retry %d times", releases)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	process, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	if signalErr := process.Signal(syscall.Signal(0)); signalErr == nil {
		t.Fatalf("cancelled harness child %d is still running", pid)
	}
}

func TestWorkerTransportClassificationIncludesRefusalTimeoutAndRetryableStatus(t *testing.T) {
	c := &client{base: "http://127.0.0.1:1", workspace: "demo"}
	_, err := c.workerConfigContext(t.Context(), "credential")
	if !transientWorkerError(err) {
		t.Fatalf("connection refusal not transient: %v", err)
	}
	if !transientWorkerError(&workerHTTPError{StatusCode: 503, Status: "503 Service Unavailable"}) {
		t.Fatal("503 not transient")
	}
	if transientWorkerError(&workerHTTPError{StatusCode: 401, Status: "401 Unauthorized"}) {
		t.Fatal("401 classified transient")
	}
	if !transientWorkerError(context.DeadlineExceeded) {
		t.Fatal("request timeout not transient")
	}
}

func TestRenewWorkerClaimRetriesWithinLeaseAndFailsClosedAfterGap(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "brief outage", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "order", State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Second)})
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo"}
	renewed, err := renewWorkerClaimUntil(t.Context(), c, "credential", "order", "session", time.Now().Add(750*time.Millisecond))
	if err != nil || calls != 2 || renewed.ID != "order" {
		t.Fatalf("brief outage renewed=%+v calls=%d err=%v", renewed, calls, err)
	}

	calls = 0
	if _, err = renewWorkerClaimUntil(t.Context(), c, "credential", "order", "session", time.Now().Add(-time.Millisecond)); !errors.Is(err, errWorkerClaimAuthorityLost) || calls != 0 {
		t.Fatalf("wake-like gap calls=%d err=%v", calls, err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		http.Error(w, "late response", http.StatusServiceUnavailable)
	}))
	defer slow.Close()
	_, err = renewWorkerClaimUntil(t.Context(), &client{base: slow.URL, workspace: "demo"}, "credential", "order", "session", time.Now().Add(40*time.Millisecond))
	if !errors.Is(err, errWorkerClaimAuthorityLost) {
		t.Fatalf("outage beyond lease err=%v", err)
	}
}

func TestProbeHarnessesRunsSlowProbesConcurrently(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CONVEYOR_PROBE_RENDEZVOUS", directory)
	harnesses := []config.Harness{
		{Name: "implement", ProbeCommand: []string{os.Args[0], "-test.run=TestWorkerSlowProbeHelper", "--", "implement"}, ProbeTimeout: 3 * time.Second},
		{Name: "review", ProbeCommand: []string{os.Args[0], "-test.run=TestWorkerSlowProbeHelper", "--", "review"}, ProbeTimeout: 3 * time.Second},
	}
	probes := probeHarnesses(t.Context(), harnesses)
	if len(probes) != 2 || !probes[0].Healthy || !probes[1].Healthy {
		t.Fatalf("probes=%+v", probes)
	}
}

func TestWorkerAPILoopProbesActiveSnapshotsAfterHotReload(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	probeCommand := func(label string) []string {
		return []string{os.Args[0], "-test.run=TestWorkerVersionedProbeHelper", "--", label}
	}
	oldCodex := config.Harness{Name: "codex", Command: []string{"codex-old", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: probeCommand("old-codex"), ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	oldClaude := config.Harness{Name: "claude", Command: []string{"claude-old", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: probeCommand("old-claude"), ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	cfg := &config.Config{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour,
		Harnesses: []config.Harness{oldCodex, oldClaude},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP},
			"review":    {Model: "gpt-review", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP},
		}},
		Review:    config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}, {Model: "claude-review", Harness: "claude"}}},
		Execution: config.ExecutionPolicy{DefaultMode: "auto", ImplementConcurrency: 1, ReviewConcurrency: 2},
		Repos:     []config.Repo{{Name: "app", Base: "main"}},
	}
	task := core.Task{ID: "worker-hot-reload", Workspace: "demo", Repo: "app", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	// Hot reload changes codex in place and removes claude. The worker must
	// probe both current and active-snapshot definitions before listing work.
	newCodex := oldCodex
	newCodex.Command = []string{"codex-new", "{prompt}", "{mcp_config}"}
	newCodex.ProbeCommand = probeCommand("new-codex")
	cfg.Harnesses = []config.Harness{newCodex}
	cfg.Review = config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "next-review", Harness: "codex"}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider, Now: func() time.Time { return now }}
	pairing, _, err := workers.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := workers.Enroll(t.Context(), pairing, "hot-reload-worker")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace, server.ConfigProvider, server.WorkOrders, server.Workers = "demo", provider, orders, workers
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := &client{base: httpServer.URL, workspace: "demo"}

	document, err := client.workerConfig(enrollment.Credential)
	if err != nil || len(document.ActiveHarnesses) != 2 {
		t.Fatalf("worker config active snapshots=%+v err=%v", document.ActiveHarnesses, err)
	}
	probeLog := t.TempDir()
	t.Setenv("CONVEYOR_VERSIONED_PROBE_LOG", probeLog)
	probes := probeWorkerConfig(t.Context(), document)
	if len(probes) != 3 {
		t.Fatalf("versioned probes=%+v", probes)
	}
	for _, label := range []string{"old-codex", "old-claude", "new-codex"} {
		if _, err := os.Stat(filepath.Join(probeLog, label)); err != nil {
			t.Fatalf("probe %s did not execute: %v", label, err)
		}
	}
	if _, err = client.heartbeatWorker(enrollment.Credential, probes); err != nil {
		t.Fatal(err)
	}
	listed, err := client.listWorkerOrders(enrollment.Credential)
	if err != nil || len(listed) != 2 {
		t.Fatalf("orders after hot reload=%+v err=%v", listed, err)
	}
	bySeat := map[int]workerservice.DispatchOrder{}
	for _, item := range listed {
		bySeat[item.Order.ReviewSeat] = item
	}
	if bySeat[1].Model != "gpt-review" || bySeat[1].Harness.Command[0] != "codex-old" || bySeat[1].Harness.ProbeCommand[len(bySeat[1].Harness.ProbeCommand)-1] != "old-codex" || bySeat[2].Model != "claude-review" || bySeat[2].Harness.Command[0] != "claude-old" {
		t.Fatalf("snapshotted dispatch orders=%+v", bySeat)
	}
	claimed, err := client.claimWorkerOrder(enrollment.Credential, bySeat[2].Order.ID, "worker-review-hot-reload", "worker-review-token")
	if err != nil || claimed.Model != "claude-review" || claimed.ModelEnforcement != "worker-pinned" {
		t.Fatalf("snapshotted claim=%+v err=%v", claimed, err)
	}
}

func TestWorkerAPILoopProbesImplementationSnapshotAfterHotReload(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	probeCommand := func(label string) []string {
		return []string{os.Args[0], "-test.run=TestWorkerVersionedProbeHelper", "--", label}
	}
	oldCodex := config.Harness{Name: "codex", Command: []string{"codex-old", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: probeCommand("old-implement-codex"), ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	cfg := &config.Config{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour,
		Harnesses: []config.Harness{oldCodex},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Model: "gpt-implement", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP},
		}},
		Execution: config.ExecutionPolicy{DefaultMode: "auto", ImplementConcurrency: 1, ReviewConcurrency: 1},
		Repos:     []config.Repo{{Name: "app", Base: "main"}},
	}
	task := core.Task{ID: "worker-implement-hot-reload", Workspace: "demo", Repo: "app", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	// The queued implementation order must keep its original harness snapshot
	// probeable and claimable after a same-name registry hot reload.
	newCodex := oldCodex
	newCodex.Command = []string{"codex-new", "{prompt}", "{mcp_config}"}
	newCodex.ProbeCommand = probeCommand("new-implement-codex")
	cfg.Harnesses = []config.Harness{newCodex}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider, Now: func() time.Time { return now }}
	pairing, _, err := workers.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := workers.Enroll(t.Context(), pairing, "implementation-hot-reload-worker")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace, server.ConfigProvider, server.WorkOrders, server.Workers = "demo", provider, orders, workers
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := &client{base: httpServer.URL, workspace: "demo"}

	document, err := client.workerConfig(enrollment.Credential)
	if err != nil || len(document.ActiveHarnesses) != 1 || document.ActiveHarnesses[0].Harness.Command[0] != "codex-old" {
		t.Fatalf("worker config implementation snapshot=%+v err=%v", document.ActiveHarnesses, err)
	}
	probeLog := t.TempDir()
	t.Setenv("CONVEYOR_VERSIONED_PROBE_LOG", probeLog)
	probes := probeWorkerConfig(t.Context(), document)
	if len(probes) != 2 {
		t.Fatalf("implementation versioned probes=%+v", probes)
	}
	for _, label := range []string{"old-implement-codex", "new-implement-codex"} {
		if _, err := os.Stat(filepath.Join(probeLog, label)); err != nil {
			t.Fatalf("probe %s did not execute: %v", label, err)
		}
	}
	if _, err = client.heartbeatWorker(enrollment.Credential, probes); err != nil {
		t.Fatal(err)
	}
	listed, err := client.listWorkerOrders(enrollment.Credential)
	if err != nil || len(listed) != 1 {
		t.Fatalf("implementation orders after hot reload=%+v err=%v", listed, err)
	}
	item := listed[0]
	if item.Order.Stage != core.StageImplement || item.Model != "gpt-implement" || item.Harness.Command[0] != "codex-old" || item.Harness.ProbeCommand[len(item.Harness.ProbeCommand)-1] != "old-implement-codex" {
		t.Fatalf("snapshotted implementation dispatch=%+v", item)
	}
	claimed, err := client.claimWorkerOrder(enrollment.Credential, item.Order.ID, "worker-implement-hot-reload", "worker-implement-token")
	if err != nil || claimed.Model != "gpt-implement" || claimed.Agent != "codex" || claimed.RequiredModel != "gpt-implement" || claimed.RequiredHarnessConfig == nil || claimed.RequiredHarnessConfig.Command[0] != "codex-old" {
		t.Fatalf("snapshotted implementation claim=%+v err=%v", claimed, err)
	}
}

func TestWorkerVersionedProbeHelper(t *testing.T) {
	directory := os.Getenv("CONVEYOR_VERSIONED_PROBE_LOG")
	if directory == "" {
		return
	}
	label := os.Args[len(os.Args)-1]
	if err := os.WriteFile(filepath.Join(directory, label), []byte("probed"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSlowProbeHelper(t *testing.T) {
	directory := os.Getenv("CONVEYOR_PROBE_RENDEZVOUS")
	if directory == "" {
		return
	}
	name := os.Args[len(os.Args)-1]
	other := "implement"
	if name == "implement" {
		other = "review"
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, other)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s did not overlap probe %s", name, other)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunHarnessChildCompletesImplementAndReviewMCPFlows(t *testing.T) {
	t.Setenv("CONVEYOR_FAKE_HARNESS", "1")
	t.Setenv("CONVEYOR_FAKE_HARNESS_EMIT_ENV", "1")
	var mu sync.Mutex
	sessions := map[string]string{}
	clientTokens := map[string]string{}
	states := map[string]core.WorkOrderState{}
	called := map[string]int{}
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/mcp" {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			orderID, _ := request.Params.Arguments["work_order_id"].(string)
			session, _ := request.Params.Arguments["session_id"].(string)
			mu.Lock()
			defer mu.Unlock()
			if sessions[orderID] == "" || sessions[orderID] != session {
				http.Error(w, "wrong session", http.StatusForbidden)
				return
			}
			called[request.Params.Name]++
			switch request.Params.Name {
			case "get_work_order":
			case "submit_for_review":
				states[orderID] = core.WorkOrderSubmitted
			case "submit_review_verdict":
				states[orderID] = core.WorkOrderCompleted
			default:
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
			payload, _ := json.Marshal(map[string]any{"ok": true})
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(payload)}}}})
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		orderID, action := parts[3], parts[4]
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "claim":
			var claim struct {
				SessionID   string `json:"session_id"`
				ClientToken string `json:"client_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			sessions[orderID] = claim.SessionID
			clientTokens[orderID] = claim.ClientToken
			states[orderID] = core.WorkOrderClaimed
		case "renew":
		case "release":
			releases++
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: orderID, State: states[orderID]})
	}))
	defer server.Close()

	c := &client{base: server.URL, workspace: "demo"}
	harness := config.Harness{Name: "fake", Command: []string{os.Args[0], "-test.run=TestWorkerHarnessHelper", "--", "{prompt}", "{mcp_config}"}}
	outputs := map[core.Stage]string{}
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		orderID := "fake-" + string(stage)
		item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: orderID, Stage: stage}, Harness: harness, Model: "fake-model"}
		var stdout, stderr bytes.Buffer
		if err := runHarnessChildWithOutput(t.Context(), c, "worker-credential", item, &stdout, &stderr); err != nil {
			t.Fatalf("%s child: %v", stage, err)
		}
		outputs[stage] = stdout.String() + stderr.String()
	}
	mu.Lock()
	defer mu.Unlock()
	if states["fake-implement"] != core.WorkOrderSubmitted || states["fake-review"] != core.WorkOrderCompleted || called["get_work_order"] != 2 || called["submit_for_review"] != 1 || called["submit_review_verdict"] != 1 || releases != 0 {
		t.Fatalf("states=%+v called=%+v releases=%d", states, called, releases)
	}
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		orderID := "fake-" + string(stage)
		output := outputs[stage]
		for _, sensitive := range []string{"worker-credential", server.URL, sessions[orderID], clientTokens[orderID]} {
			if sensitive != "" && strings.Contains(output, sensitive) {
				t.Fatalf("%s child output leaked runtime value: %q", stage, output)
			}
		}
		if strings.Count(output, "[REDACTED:exact]") < 4 {
			t.Fatalf("%s child output was not fully redacted: %q", stage, output)
		}
	}
}

func TestRunHarnessChildReviewExitReconciliation(t *testing.T) {
	for _, harnessName := range []string{"codex", "claude"} {
		for _, scenario := range []struct {
			name         string
			mode         string
			wantError    bool
			wantState    core.WorkOrderState
			wantReleases int
			wantSubmits  int
		}{
			{name: "successful tool submission", mode: "submit", wantState: core.WorkOrderCompleted, wantSubmits: 1},
			{name: "successful submission before failure exit", mode: "submit-failure", wantState: core.WorkOrderCompleted, wantSubmits: 1},
			{name: "JSON-only final output", mode: "json-only", wantError: true, wantState: core.WorkOrderQueued, wantReleases: 1},
			{name: "clean exit without submission", mode: "clean", wantError: true, wantState: core.WorkOrderQueued, wantReleases: 1},
			{name: "failure exit", mode: "failure", wantError: true, wantState: core.WorkOrderQueued, wantReleases: 1},
		} {
			t.Run(harnessName+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("CONVEYOR_FAKE_HARNESS", "1")
				t.Setenv("CONVEYOR_FAKE_HARNESS_MODE", scenario.mode)
				var mu sync.Mutex
				state := core.WorkOrderQueued
				session := ""
				releaseReasons := []string{}
				submits := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer worker-credential" {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					if r.URL.Path == "/mcp" {
						var request struct {
							ID     json.RawMessage `json:"id"`
							Params struct {
								Name      string         `json:"name"`
								Arguments map[string]any `json:"arguments"`
							} `json:"params"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						if request.Params.Arguments["session_id"] != session {
							http.Error(w, "wrong session", http.StatusForbidden)
							return
						}
						switch request.Params.Name {
						case "get_work_order":
						case "submit_review_verdict":
							submits++
							state = core.WorkOrderCompleted
						default:
							http.Error(w, "unexpected tool", http.StatusBadRequest)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
						return
					}
					parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
					if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
						http.NotFound(w, r)
						return
					}
					switch parts[4] {
					case "claim":
						var claim struct {
							SessionID string `json:"session_id"`
						}
						_ = json.NewDecoder(r.Body).Decode(&claim)
						session, state = claim.SessionID, core.WorkOrderClaimed
					case "renew":
					case "release":
						var release struct {
							Reason string `json:"reason"`
						}
						_ = json.NewDecoder(r.Body).Decode(&release)
						releaseReasons = append(releaseReasons, release.Reason)
						state = core.WorkOrderQueued
					default:
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: state})
				}))
				defer server.Close()

				item := workerservice.DispatchOrder{
					Order:   core.WorkOrder{ID: "review-" + harnessName + "-exit", Stage: core.StageReview},
					Harness: config.Harness{Name: harnessName, Command: []string{os.Args[0], "-test.run=TestWorkerHarnessHelper", "--", "{prompt}", "{mcp_config}"}},
					Model:   harnessName + "-review",
				}
				err := runHarnessChild(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item)
				if (err != nil) != scenario.wantError {
					t.Fatalf("run error=%v wantError=%v", err, scenario.wantError)
				}
				mu.Lock()
				defer mu.Unlock()
				if state != scenario.wantState || len(releaseReasons) != scenario.wantReleases || submits != scenario.wantSubmits {
					t.Fatalf("state=%s releases=%q submits=%d", state, releaseReasons, submits)
				}
				for _, reason := range releaseReasons {
					if reason != "harness exited without terminal verdict submission" {
						t.Fatalf("release reason=%q", reason)
					}
				}
			})
		}
	}
}

func TestRunHarnessChildReviewCanRetryAfterExitRelease(t *testing.T) {
	t.Setenv("CONVEYOR_FAKE_HARNESS", "1")
	t.Setenv("CONVEYOR_FAKE_HARNESS_MODE", "clean")
	var mu sync.Mutex
	state := core.WorkOrderQueued
	session := ""
	releases, submits := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/mcp" {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Params.Arguments["session_id"] != session {
				http.Error(w, "wrong session", http.StatusForbidden)
				return
			}
			if request.Params.Name == "submit_review_verdict" {
				submits++
				state = core.WorkOrderCompleted
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch parts[4] {
		case "claim":
			var claim struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			session, state = claim.SessionID, core.WorkOrderClaimed
		case "renew":
		case "release":
			releases++
			state = core.WorkOrderQueued
		}
		_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: state})
	}))
	defer server.Close()
	item := workerservice.DispatchOrder{
		Order:   core.WorkOrder{ID: "review-retry", Stage: core.StageReview},
		Harness: config.Harness{Name: "claude", Command: []string{os.Args[0], "-test.run=TestWorkerHarnessHelper", "--", "{prompt}", "{mcp_config}"}},
		Model:   "claude-review",
	}
	c := &client{base: server.URL, workspace: "demo"}
	if err := runHarnessChild(t.Context(), c, "worker-credential", item); err == nil {
		t.Fatal("clean exit without submission succeeded")
	}
	t.Setenv("CONVEYOR_FAKE_HARNESS_MODE", "submit")
	if err := runHarnessChild(t.Context(), c, "worker-credential", item); err != nil {
		t.Fatalf("retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if state != core.WorkOrderCompleted || releases != 1 || submits != 1 {
		t.Fatalf("state=%s releases=%d submits=%d", state, releases, submits)
	}
}

func TestRunHarnessChildMaterializesSpecRepositoryOutsideWorkerDirectory(t *testing.T) {
	fixture := newGitFixture(t)
	wantHead := mustGitOutput(t, fixture.seed, "rev-parse", "HEAD")
	workerDirectory := t.TempDir()
	t.Chdir(workerDirectory)
	reportPath := filepath.Join(t.TempDir(), "spec-repository.json")
	t.Setenv("CONVEYOR_FAKE_HARNESS", "1")
	t.Setenv("CONVEYOR_FAKE_HARNESS_REPO_REPORT", reportPath)

	var mu sync.Mutex
	state := core.WorkOrderQueued
	session := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/mcp" {
			var request struct {
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Params.Arguments["session_id"] != session {
				http.Error(w, "wrong session", http.StatusForbidden)
				return
			}
			if request.Params.Name == "submit_plan" {
				state = core.WorkOrderCompleted
			} else if request.Params.Name != "get_work_order" {
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim":
			var claim struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			session, state = claim.SessionID, core.WorkOrderClaimed
		case "reconcile":
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: parts[3], State: state}, Authorized: state == core.WorkOrderClaimed})
			return
		case "release", "renew":
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: state, LeaseExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{
		Order:      core.WorkOrder{ID: "fake-spec", Stage: core.StageSpec},
		Task:       core.Task{ID: "task-spec", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-spec"},
		Repository: config.Repo{Name: "app", URL: fixture.origin, Base: "main"},
		Harness:    config.Harness{Name: "fake", Command: []string{os.Args[0], "-test.run=TestWorkerHarnessHelper", "--", "{prompt}", "{mcp_config}"}},
		Model:      "fake-model",
	}
	var stdout, stderr bytes.Buffer
	if err := runHarnessChildWithOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Directory string `json:"directory"`
		Head      string `json:"head"`
		Branch    string `json:"branch"`
		Readme    string `json:"readme"`
		PushURL   string `json:"push_url"`
		Mode      uint32 `json:"mode"`
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Directory == workerDirectory || report.Head != wantHead || report.Branch != "" || report.Readme != "main\n" || report.PushURL != "disabled://conveyor-spec-read-only" || report.Mode&0o222 != 0 {
		t.Fatalf("spec repository report=%+v worker_directory=%s want_head=%s", report, workerDirectory, wantHead)
	}
}

func TestRunHarnessChildFirstActivityTimeoutReapsSilentHarnessProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "silent-harness.pid")
	grandchildPIDFile := filepath.Join(t.TempDir(), "silent-grandchild.pid")
	t.Setenv("CONVEYOR_FAKE_HARNESS_PID_FILE", pidFile)
	t.Setenv("CONVEYOR_FAKE_HARNESS_GRANDCHILD_PID_FILE", grandchildPIDFile)
	releases := make(chan core.WorkOrderRelease, 2)
	checkpoints := make(chan core.WorkOrderAttemptCheckpoint, 1)
	previousCheckpointer := workerAttemptCheckpointer
	workerAttemptCheckpointer = func(_ context.Context, _, _, _ string, checkpoint attemptCheckpoint) (*attemptCheckpointResult, error) {
		if checkpoint.AttemptID != "attempt-silent" || checkpoint.WorkOrderID != "silent-first-activity" || checkpoint.TerminationReason != workerFirstActivityTimeoutReason {
			t.Fatalf("checkpoint metadata=%+v", checkpoint)
		}
		return &attemptCheckpointResult{Worktree: "/assigned/task", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Pushed: true}, nil
	}
	t.Cleanup(func() { workerAttemptCheckpointer = previousCheckpointer })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim", "renew":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, AttemptID: "attempt-silent", LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "attempt-checkpoint":
			var checkpoint core.WorkOrderAttemptCheckpoint
			if err := json.NewDecoder(r.Body).Decode(&checkpoint); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			checkpoints <- checkpoint
			_ = json.NewEncoder(w).Encode(map[string]bool{"created": true})
		case "release":
			var release core.WorkOrderRelease
			if err := json.NewDecoder(r.Body).Decode(&release); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			releases <- release
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{
		Order:      core.WorkOrder{ID: "silent-first-activity", Stage: core.StageImplement},
		Task:       core.Task{ID: "silent-task", Branch: "conveyor/silent-task", Repo: "conveyor"},
		Repository: config.Repo{Name: "conveyor", URL: "https://github.com/kidus-tiliksew/conveyor.git"},
		Harness:    config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "silent-grandchild"}},
	}
	var stdout, stderr bytes.Buffer
	err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, 500*time.Millisecond, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), workerFirstActivityTimeoutReason) {
		t.Fatalf("timeout error=%v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("silent harness emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	select {
	case checkpoint := <-checkpoints:
		if checkpoint.AttemptID != "attempt-silent" || checkpoint.TerminationReason != workerFirstActivityTimeoutReason || checkpoint.PushResult != "pushed" {
			t.Fatalf("checkpoint request=%+v", checkpoint)
		}
	default:
		t.Fatal("silent harness checkpoint was not recorded before release")
	}
	select {
	case release := <-releases:
		if release.Outcome != core.WorkOrderOutcomeChildFailure || release.Reason != workerFirstActivityTimeoutReason {
			t.Fatalf("release=%+v", release)
		}
	default:
		t.Fatal("silent harness was not released")
	}
	select {
	case duplicate := <-releases:
		t.Fatalf("silent harness was released more than once: %+v", duplicate)
	default:
	}
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("silent harness process %d was not reaped: %v", pid, err)
	}
	grandchildPIDData, err := os.ReadFile(grandchildPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(string(grandchildPIDData)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(grandchildPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("silent harness grandchild %d was not reaped: %v", grandchildPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunHarnessChildFirstActivityDisarmsTimeoutWithoutAddingSilenceLimit(t *testing.T) {
	released := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim", "renew":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "reconcile":
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: parts[3], State: core.WorkOrderSubmitted}, Reason: "submitted"})
		case "release":
			released <- struct{}{}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, mode := range []string{"early-output", "early-error"} {
		t.Run(mode, func(t *testing.T) {
			item := workerservice.DispatchOrder{
				Order:   core.WorkOrder{ID: "active-first-activity-" + mode, Stage: core.StageImplement},
				Harness: config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", mode}},
			}
			var stdout, stderr bytes.Buffer
			started := time.Now()
			if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, 100*time.Millisecond, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
				t.Fatalf("active harness did not remain running beyond first activity timeout: %s", elapsed)
			}
			select {
			case <-released:
				t.Fatalf("active harness was released: stdout=%q stderr=%q", stdout.String(), stderr.String())
			default:
			}
			if !strings.Contains(stdout.String()+stderr.String(), "first activity") {
				t.Fatalf("active harness stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunHarnessChildStallTimeoutStopsAndReleasesSilentChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "stalled-harness.pid")
	t.Setenv("CONVEYOR_FAKE_HARNESS_PID_FILE", pidFile)
	releases := make(chan core.WorkOrderRelease, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim", "renew":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "release":
			var release core.WorkOrderRelease
			if err := json.NewDecoder(r.Body).Decode(&release); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			releases <- release
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "stalled-silent-child", Stage: core.StageImplement},
		Harness: config.Harness{
			Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "silent"},
			StallTimeoutText: "150ms",
		},
	}
	var stdout, stderr bytes.Buffer
	err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, time.Second, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), workerStallTimeoutReason) {
		t.Fatalf("stall error=%v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("silent harness produced output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	select {
	case release := <-releases:
		if release.Outcome != core.WorkOrderOutcomeStalled || release.Reason != workerStallTimeoutReason {
			t.Fatalf("release=%+v", release)
		}
	default:
		t.Fatal("stalled harness was not released")
	}
	select {
	case duplicate := <-releases:
		t.Fatalf("stalled harness was released more than once: %+v", duplicate)
	default:
	}
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stalled harness process %d was not reaped: %v", pid, err)
	}
}

func TestRunHarnessChildContinuousOutputResetsStallTimeout(t *testing.T) {
	released := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim", "renew":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "reconcile":
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: parts[3], State: core.WorkOrderSubmitted}, Reason: "submitted"})
		case "release":
			released <- struct{}{}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "continuous-output", Stage: core.StageImplement},
		Harness: config.Harness{
			Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "continuous-output"},
			StallTimeoutText: "100ms",
		},
	}
	var stdout, stderr bytes.Buffer
	started := time.Now()
	if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, time.Second, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("continuous child ended too quickly to exercise stall resets: %s", elapsed)
	}
	if strings.Count(stdout.String(), "activity") < 6 {
		t.Fatalf("continuous output=%q", stdout.String())
	}
	select {
	case <-released:
		t.Fatal("continuously active harness was released")
	default:
	}
}

func TestRunHarnessChildOutputRacingStallDeadlineStartsNewGeneration(t *testing.T) {
	raceDirectory := t.TempDir()
	t.Setenv("CONVEYOR_FAKE_HARNESS_RACE_DIR", raceDirectory)
	released := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim", "renew":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "reconcile":
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: parts[3], State: core.WorkOrderSubmitted}, Reason: "submitted"})
		case "release":
			released <- struct{}{}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var boundary sync.Once
	workerStallDeadlineTestHook = func() {
		boundary.Do(func() {
			if err := os.WriteFile(filepath.Join(raceDirectory, "emit"), []byte("emit"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitForWorkerHelperFile(t, filepath.Join(raceDirectory, "emitted"))
			// Hold the stale timer handler past the replacement deadline. The
			// raced output must still establish a fresh timer generation.
			time.Sleep(125 * time.Millisecond)
			if err := os.WriteFile(filepath.Join(raceDirectory, "finish"), []byte("finish"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Cleanup(func() { workerStallDeadlineTestHook = nil })

	item := workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "stall-deadline-race", Stage: core.StageImplement},
		Harness: config.Harness{
			Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "stall-deadline-race"},
			StallTimeoutText: "100ms",
		},
	}
	var stdout, stderr bytes.Buffer
	if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, time.Second, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "initial activity") || !strings.Contains(stdout.String(), "deadline activity") {
		t.Fatalf("race output=%q", stdout.String())
	}
	select {
	case <-released:
		t.Fatal("deadline-racing output was misclassified as stalled")
	default:
	}
}

func TestRunHarnessChildClassifiesImmediateExitAndCancellation(t *testing.T) {
	test := func(t *testing.T, mode string, wantOutcome string, wantExit *int) {
		t.Helper()
		claimed := make(chan struct{}, 1)
		released := make(chan core.WorkOrderRelease, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
				http.NotFound(w, r)
				return
			}
			switch parts[4] {
			case "claim":
				claimed <- struct{}{}
				_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed})
			case "release":
				var request core.WorkOrderRelease
				var wire struct {
					Reason     string `json:"reason"`
					Cause      string `json:"release_cause"`
					Outcome    string `json:"outcome"`
					ExitStatus *int   `json:"exit_status"`
				}
				_ = json.NewDecoder(r.Body).Decode(&wire)
				request.Reason, request.Cause, request.Outcome, request.ExitStatus = wire.Reason, wire.Cause, wire.Outcome, wire.ExitStatus
				released <- request
				_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "classified-" + mode, Stage: core.StageReview}, Harness: config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", mode}}}
		done := make(chan error, 1)
		var stdout, stderr bytes.Buffer
		go func() {
			done <- runHarnessChildWithOutput(ctx, &client{base: server.URL, workspace: "demo"}, "worker-credential", item, &stdout, &stderr)
		}()
		<-claimed
		if mode == "cancel" {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}
		if err := <-done; err == nil {
			t.Fatal("child unexpectedly succeeded")
		}
		if mode == "exit" {
			output := stdout.String() + stderr.String()
			if strings.Contains(output, "worker-credential") || strings.Contains(output, server.URL) || strings.Count(output, "[REDACTED:exact]") < 4 {
				t.Fatalf("failed child leaked runtime output: %q", output)
			}
		}
		select {
		case release := <-released:
			if release.Outcome != wantOutcome || release.Cause != core.WorkOrderReleaseCauseSessionExit || !reflect.DeepEqual(release.ExitStatus, wantExit) {
				t.Fatalf("release=%+v want outcome=%s exit=%v", release, wantOutcome, wantExit)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("release was not reported")
		}
	}
	exit := 7
	t.Run("immediate exit", func(t *testing.T) { test(t, "exit", core.WorkOrderOutcomeChildFailure, &exit) })
	t.Run("cancellation", func(t *testing.T) { test(t, "cancel", core.WorkOrderOutcomeCancelled, nil) })
}

func TestRunHarnessChildRenewsClaimDuringSlowPreStartSetup(t *testing.T) {
	previousInterval := workerClaimRenewInterval
	workerClaimRenewInterval = 100 * time.Millisecond
	workerPreStartTestHook = func(context.Context) { time.Sleep(1200 * time.Millisecond) }
	t.Cleanup(func() {
		workerClaimRenewInterval = previousInterval
		workerPreStartTestHook = nil
	})

	// The fake control plane expires unrenewed leases like the real one: a
	// pre-start setup slower than the lease window only survives when the
	// worker renews between claim and child launch.
	const lease = 750 * time.Millisecond
	var mu sync.Mutex
	renews := 0
	var leaseDeadline time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch parts[4] {
		case "claim":
			leaseDeadline = time.Now().Add(lease)
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: leaseDeadline})
		case "renew":
			if time.Now().After(leaseDeadline) {
				http.Error(w, "lease expired", http.StatusConflict)
				return
			}
			renews++
			leaseDeadline = time.Now().Add(lease)
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: leaseDeadline})
		case "reconcile":
			reconciliation := workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: parts[3], State: core.WorkOrderSubmitted}, Reason: "submitted"}
			if time.Now().After(leaseDeadline) {
				reconciliation.WorkOrder.State = core.WorkOrderQueued
				reconciliation.Reason = "lease expired without renewal"
			}
			_ = json.NewEncoder(w).Encode(reconciliation)
		case "release":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "slow-setup", Stage: core.StageImplement}, Harness: config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "ok"}}}
	var stdout, stderr bytes.Buffer
	if err := runHarnessChildWithOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, &stdout, &stderr); err != nil {
		t.Fatalf("slow pre-start setup lost the claim: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if renews < 2 {
		t.Fatalf("claim was not renewed during pre-start setup: renews=%d", renews)
	}
}

func TestRunHarnessChildAuthorityLossDuringPreStartSetupAbortsLaunch(t *testing.T) {
	previousInterval := workerClaimRenewInterval
	workerClaimRenewInterval = 50 * time.Millisecond
	workerPreStartTestHook = func(ctx context.Context) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Cleanup(func() {
		workerClaimRenewInterval = previousInterval
		workerPreStartTestHook = nil
	})

	released := make(chan core.WorkOrderRelease, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed, LeaseExpiresAt: time.Now().Add(10 * time.Second)})
		case "renew":
			// The server has revoked this claim; renewal reports the order
			// back in queue.
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		case "release":
			var request core.WorkOrderRelease
			_ = json.NewDecoder(r.Body).Decode(&request)
			released <- request
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "authority-lost", Stage: core.StageImplement}, Harness: config.Harness{Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "exit"}}}
	var stdout, stderr bytes.Buffer
	err := runHarnessChildWithOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", item, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "server reports queued") {
		t.Fatalf("authority loss during setup was not surfaced: %v", err)
	}
	select {
	case release := <-released:
		if release.Outcome != core.WorkOrderOutcomeReleased || !strings.Contains(release.Reason, "claim authority lost") {
			t.Fatalf("release=%+v want released with claim-authority-lost reason", release)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release was not reported")
	}
	// The "exit" helper writes marker lines before exiting; setup aborted
	// before launch, so no child output may exist.
	if output := stdout.String() + stderr.String(); output != "" {
		t.Fatalf("child was launched despite authority loss: %q", output)
	}
}

func TestRunHarnessChildReadinessFailureReleasesClaimWithoutStartingModel(t *testing.T) {
	grok := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(grok, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	released := make(chan core.WorkOrderRelease, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "claim":
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderClaimed})
		case "release":
			var request core.WorkOrderRelease
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			released <- request
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: parts[3], State: core.WorkOrderQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_CLIENT_TOKEN", "parent-client-token")
	harness := config.Harness{
		Name: "grok", MCPTransport: config.MCPTransportEnvironment, MCPAttachment: "conveyor",
		Command: []string{grok, "{prompt}"}, ProbeCommand: []string{grok, "--version"}, ProbeTimeoutText: "1s",
	}
	var stdout, stderr bytes.Buffer
	err := runHarnessChildWithOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "worker-credential", workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "readiness-failure", Stage: core.StageImplement}, Harness: harness,
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "readiness inspection failed") {
		t.Fatalf("readiness error=%v", err)
	}
	select {
	case release := <-released:
		if release.Outcome != core.WorkOrderOutcomeReleased || release.Reason != "environment MCP readiness failed" {
			t.Fatalf("release=%+v", release)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readiness failure did not release the claim")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 || os.Getenv("CONVEYOR_CLIENT_TOKEN") != "parent-client-token" {
		t.Fatalf("readiness failure leaked output or mutated parent launch state")
	}
}

func TestWorkerLifecycleHelper(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if pidFile := os.Getenv("CONVEYOR_FAKE_HARNESS_PID_FILE"); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	switch mode {
	case "exit":
		fmt.Fprintf(os.Stdout, "token=%s address=%s\n", os.Getenv("CONVEYOR_API_TOKEN"), os.Getenv("CONVEYOR_ADDR"))
		fmt.Fprintf(os.Stderr, "session=%s client=%s\n", os.Getenv("CONVEYOR_SESSION_ID"), os.Getenv("CONVEYOR_CLIENT_TOKEN"))
		os.Exit(7)
	case "cancel":
		time.Sleep(30 * time.Second)
	case "silent":
		time.Sleep(30 * time.Second)
	case "silent-grandchild":
		grandchild := exec.Command(os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", "cancel")
		grandchild.Env = append(os.Environ(), "CONVEYOR_FAKE_HARNESS_PID_FILE=")
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("CONVEYOR_FAKE_HARNESS_GRANDCHILD_PID_FILE"), []byte(strconv.Itoa(grandchild.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
	case "early-output":
		fmt.Fprintln(os.Stdout, "first activity")
		time.Sleep(400 * time.Millisecond)
	case "early-error":
		fmt.Fprintln(os.Stderr, "first activity")
		time.Sleep(400 * time.Millisecond)
	case "early-then-silent":
		fmt.Fprintln(os.Stdout, "first activity")
		time.Sleep(30 * time.Second)
	case "continuous-output":
		for i := 0; i < 8; i++ {
			fmt.Fprintf(os.Stdout, "activity %d\n", i)
			time.Sleep(40 * time.Millisecond)
		}
	case "stall-deadline-race":
		raceDirectory := os.Getenv("CONVEYOR_FAKE_HARNESS_RACE_DIR")
		fmt.Fprintln(os.Stdout, "initial activity")
		waitForWorkerHelperFile(t, filepath.Join(raceDirectory, "emit"))
		fmt.Fprintln(os.Stdout, "deadline activity")
		if err := os.WriteFile(filepath.Join(raceDirectory, "emitted"), []byte("emitted"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForWorkerHelperFile(t, filepath.Join(raceDirectory, "finish"))
		time.Sleep(25 * time.Millisecond)
	}
}

func waitForWorkerHelperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestWorkerHarnessHelper(t *testing.T) {
	if os.Getenv("CONVEYOR_FAKE_HARNESS") != "1" {
		return
	}
	if len(os.Args) < 3 {
		t.Fatal("missing prompt and MCP config arguments")
	}
	if os.Getenv("CONVEYOR_FAKE_HARNESS_EMIT_ENV") == "1" {
		fmt.Fprintf(os.Stdout, "token=%s address=%s\n", os.Getenv("CONVEYOR_API_TOKEN"), os.Getenv("CONVEYOR_ADDR"))
		fmt.Fprintf(os.Stderr, "session=%s client=%s\n", os.Getenv("CONVEYOR_SESSION_ID"), os.Getenv("CONVEYOR_CLIENT_TOKEN"))
	}
	prompt, configPath := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	session, orderID := os.Getenv("CONVEYOR_SESSION_ID"), os.Getenv("CONVEYOR_WORK_ORDER_ID")
	if session == "" || !strings.Contains(prompt, session) {
		t.Fatalf("prompt does not carry exact session_id %q: %s", session, prompt)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	server := document.Servers["conveyor"]
	call := func(name string, arguments map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
		request, requestErr := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", server.Headers["Authorization"])
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %s", name, response.Status)
		}
	}
	identity := map[string]any{"workspace_id": os.Getenv("CONVEYOR_WORKSPACE"), "work_order_id": orderID, "session_id": session}
	call("get_work_order", identity)
	if reportPath := os.Getenv("CONVEYOR_FAKE_HARNESS_REPO_REPORT"); reportPath != "" {
		directory, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		readme, err := os.ReadFile("README.md")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(".")
		if err != nil {
			t.Fatal(err)
		}
		report, _ := json.Marshal(map[string]any{
			"directory": directory,
			"head":      mustGitOutput(t, directory, "rev-parse", "HEAD"),
			"branch":    mustGitOutput(t, directory, "branch", "--show-current"),
			"readme":    string(readme),
			"push_url":  mustGitOutput(t, directory, "remote", "get-url", "--push", "origin"),
			"mode":      uint32(info.Mode().Perm()),
		})
		if err = os.WriteFile(reportPath, report, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(orderID, "spec") {
		identity["markdown"] = "## Approach\nGrounded plan.\n\n## Files touched\n- README.md\n\n## Ordering\n1. Verify the repository.\n\n## Risks\n- None.\n\n## Done criteria\n- The repository is verified."
		identity["decomposition"] = []any{}
		call("submit_plan", identity)
	} else if strings.Contains(orderID, "implement") {
		call("submit_for_review", identity)
	} else {
		mode := os.Getenv("CONVEYOR_FAKE_HARNESS_MODE")
		switch mode {
		case "json-only":
			fmt.Fprintln(os.Stdout, `{"verdict":"approve","reason_code":"approved","summary":"printed but not submitted"}`)
			return
		case "clean":
			return
		case "failure":
			t.Fatal("fake review harness failed before verdict submission")
		}
		identity["verdict"] = "approve"
		identity["reason_code"] = "fake"
		identity["summary"] = fmt.Sprintf("reviewed %s", orderID)
		call("submit_review_verdict", identity)
		if mode == "submit-failure" {
			t.Fatal("fake review harness failed after verdict submission")
		}
	}
}
