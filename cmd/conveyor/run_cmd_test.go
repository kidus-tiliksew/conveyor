package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type promptSignalWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *promptSignalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "Proceed with implement?") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestConfirmRunStageCancellationDeclinesWithoutWaitingForInput(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	ctx, cancel := context.WithCancel(t.Context())
	output := &promptSignalWriter{ready: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		confirmed, err := confirmRunStage(ctx, bufio.NewReader(reader), output, core.StageImplement)
		if confirmed {
			result <- fmt.Errorf("cancelled prompt was confirmed")
			return
		}
		result <- err
	}()

	select {
	case <-output.ready:
	case <-time.After(time.Second):
		t.Fatal("prompt was not written")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled prompt remained blocked on input")
	}
}

func TestConfiguredFirstActivityTimeoutRejectsInvalidAndNonPositiveText(t *testing.T) {
	for _, value := range []string{"eventually", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			local := &config.Config{Execution: config.ExecutionPolicy{FirstActivityTimeoutText: value}}
			if _, err := configuredFirstActivityTimeout(local); err == nil || !strings.Contains(err.Error(), "execution.first_activity_timeout") {
				t.Fatalf("timeout %q error=%v", value, err)
			}
		})
	}
}

type taskRunStats struct {
	requests                                         int
	stderr                                           string
	states                                           map[string]core.WorkOrderState
	getCalls, claimCalls, planSubmits, reviewSubmits int
	verdictSubmits, releaseCalls                     int
	agentIssues, agentRevokes                        int
	cleanupChecks, cleanupRecords, localCleanups     int
	progress                                         []string
	mcpCredentials                                   []string
}

func runTaskScenario(t *testing.T, input string, step, terminal bool, commandFlags ...[]string) (taskRunStats, string, error) {
	t.Helper()
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fixture := newGitFixture(t)
	t.Chdir(previousDirectory)
	t.Setenv("CONVEYOR_FAKE_TASK_RUN_HARNESS", "1")
	var mu sync.Mutex
	stats := taskRunStats{states: map[string]core.WorkOrderState{
		"target-implement-1": core.WorkOrderQueued,
		"target-review-1":    core.WorkOrderQueued,
	}}
	sessions := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization != "Bearer user-credential" && !(r.URL.Path == "/mcp" && authorization == "Bearer child-agent-credential") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		stats.requests++
		if r.URL.Path == "/mcp" {
			stats.mcpCredentials = append(stats.mcpCredentials, authorization)
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			orderID, _ := request.Params.Arguments["work_order_id"].(string)
			if request.Params.Arguments["session_id"] != sessions[orderID] || stats.states[orderID] == "" {
				http.Error(w, "wrong task session", http.StatusForbidden)
				return
			}
			switch request.Params.Name {
			case "get_work_order":
			case "report_progress":
				stats.progress = append(stats.progress, request.Params.Arguments["message"].(string))
			case "submit_for_review":
				if orderID != "target-implement-1" {
					http.Error(w, "wrong implement order", http.StatusBadRequest)
					return
				}
				stats.reviewSubmits++
				stats.states[orderID] = core.WorkOrderSubmitted
			case "submit_review_verdict":
				if orderID != "target-review-1" {
					http.Error(w, "wrong review order", http.StatusBadRequest)
					return
				}
				stats.verdictSubmits++
				stats.states[orderID] = core.WorkOrderCompleted
			default:
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forge-token":
			_, _ = io.WriteString(w, `{"configured":true,"forge_login":"owner"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks":
			_ = json.NewEncoder(w).Encode([]core.Task{})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target":
			_ = json.NewEncoder(w).Encode(core.Task{ID: "target", Title: "Ship target", State: core.TaskRunning, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/worktree-cleanup":
			stats.cleanupChecks++
			_ = json.NewEncoder(w).Encode(terminalCleanupStatus{Terminal: true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/target/worktree-cleanup":
			stats.cleanupRecords++
			_ = json.NewEncoder(w).Encode(terminalCleanupReceipt{Completed: true, Recorded: true})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-credential"):
			stats.agentIssues++
			_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "agent-id", "credential": "child-agent-credential"})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/agent-credential"):
			stats.agentRevokes++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			stats.getCalls++
			order := core.WorkOrder{ID: "target-implement-1", TaskID: "target", Stage: core.StageImplement, State: stats.states["target-implement-1"]}
			if stats.states["target-implement-1"] == core.WorkOrderSubmitted {
				order = core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: stats.states["target-review-1"], ReviewSeat: 1}
			}
			if stats.states["target-review-1"] == core.WorkOrderCompleted {
				_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
					Task:     core.Task{ID: "target", Title: "Ship target", State: core.TaskMerged, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"},
					Dispatch: "run", Auth: "user",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order:      order,
				Repository: config.Repo{Name: "conveyor", URL: fixture.origin, Base: "main"},
				Task:       core.Task{ID: "target", Title: "Ship target", State: core.TaskRunning, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"},
				Dispatch:   "run", Auth: "user",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/worktree-handoff"):
			writeTestWriterAdmission(w, r, core.WorktreeIdentity{Workspace: "demo", TaskID: "target", Repository: "file:" + fixture.origin, Branch: "conveyor/task-target", WorkOrderID: "target-implement-1", AttemptID: "attempt-target-implement-1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			stats.claimCalls++
			var claim struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			orderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/target/run-orders/"), "/claim")
			sessions[orderID], stats.states[orderID] = claim.SessionID, core.WorkOrderClaimed
			stage := core.StageImplement
			if orderID == "target-review-1" {
				stage = core.StageReview
			}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: orderID, TaskID: "target", Stage: stage, State: stats.states[orderID], AttemptID: "attempt-" + orderID, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/renew"):
			orderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/target/run-orders/"), "/renew")
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: orderID, TaskID: "target", State: stats.states[orderID], LeaseExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reconcile"):
			orderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/target/run-orders/"), "/reconcile")
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: orderID, State: stats.states[orderID]}, Authorized: stats.states[orderID] == core.WorkOrderClaimed})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			stats.releaseCalls++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	command := fakeClaudeCommandYAML(t, "TestTaskRunHarnessHelper")
	localConfig := strings.Replace(string(template), exampleHarnessCommand, command, 1)
	localConfig = strings.ReplaceAll(localConfig, "effort: high", `effort: ""`)
	localConfig = withHealthyRunProbe(t, localConfig)
	if localConfig == string(template) {
		t.Fatal("local harness command fixture was not replaced")
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(configPath, []byte(localConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.primary)
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	previousCleanup := cleanupTerminalTaskWorktree
	cleanupTerminalTaskWorktree = func(context.Context, *config.Config, workerservice.DispatchOrder) (worktreeCleanupResult, error) {
		mu.Lock()
		defer mu.Unlock()
		stats.localCleanups++
		return worktreeCleanupResult{Worktree: "removed", Branch: "retained", Path: "/worktrees/target"}, nil
	}
	t.Cleanup(func() { cleanupTerminalTaskWorktree = previousCleanup })
	var output bytes.Buffer
	if len(commandFlags) == 0 {
		err = runTask(t.Context(), c, "target", configPath, strings.NewReader(input), &output, step, terminal)
	} else {
		t.Setenv("CONVEYOR_ADDR", server.URL)
		t.Setenv("CONVEYOR_API_TOKEN", "user-credential")
		t.Setenv("CONVEYOR_WORKSPACE", "demo")
		t.Setenv(localGitTokenEnv, "")
		command := runCmd()
		command.SilenceErrors = true
		command.SilenceUsage = true
		command.SetArgs(append([]string{"target", "--config", configPath}, commandFlags[0]...))
		command.SetIn(strings.NewReader(input))
		command.SetOut(&output)
		var stderr bytes.Buffer
		command.SetErr(&stderr)
		err = command.ExecuteContext(t.Context())
		stats.stderr = stderr.String()
	}
	mu.Lock()
	defer mu.Unlock()
	return stats, output.String(), err
}

// specGateProjection is the live run-order projection the server returns once
// a submitted plan is waiting at the spec approval gate.
func specGateProjection() *workerservice.DispatchOrder {
	return &workerservice.DispatchOrder{
		Task:     core.Task{ID: "target", Title: "Plan target", State: core.TaskAwaiting, Repo: "conveyor", BaseBranch: "main"},
		Gate:     &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan v1", SpecVersion: 1},
		Dispatch: "run", Auth: "user",
	}
}

func runSpecTaskScenario(t *testing.T, input string, step, terminal bool) (taskRunStats, string, error) {
	t.Helper()
	return runSpecTaskScenarioAfter(t, input, step, terminal, specGateProjection())
}

// runSpecTaskScenarioAfter runs a spec stage and then serves after as the live
// projection; nil serves an empty (no content) projection.
func runSpecTaskScenarioAfter(t *testing.T, input string, step, terminal bool, after *workerservice.DispatchOrder) (taskRunStats, string, error) {
	t.Helper()
	t.Setenv("CONVEYOR_FAKE_TASK_RUN_HARNESS", "1")
	origin := filepath.Join(t.TempDir(), "origin.git")
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	mustGit(t, "", "init", "-b", "main", seed)
	configureGitUser(t, seed)
	writeFile(t, filepath.Join(seed, "README.md"), "spec context\n")
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "initial")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "-u", "origin", "main")

	var mu sync.Mutex
	stats := taskRunStats{states: map[string]core.WorkOrderState{"target-spec-1": core.WorkOrderQueued}}
	sessions := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization != "Bearer user-credential" && !(r.URL.Path == "/mcp" && authorization == "Bearer child-agent-credential") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/mcp" {
			stats.mcpCredentials = append(stats.mcpCredentials, authorization)
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			orderID, _ := request.Params.Arguments["work_order_id"].(string)
			if request.Params.Arguments["session_id"] != sessions[orderID] || orderID != "target-spec-1" {
				http.Error(w, "wrong task session", http.StatusForbidden)
				return
			}
			switch request.Params.Name {
			case "get_work_order":
			case "report_progress":
				stats.progress = append(stats.progress, request.Params.Arguments["message"].(string))
			case "submit_plan":
				stats.planSubmits++
				stats.states[orderID] = core.WorkOrderCompleted
			default:
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target":
			_ = json.NewEncoder(w).Encode(core.Task{ID: "target", Title: "Plan target", State: core.TaskRunning, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-credential"):
			stats.agentIssues++
			_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "agent-id", "credential": "child-agent-credential"})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/agent-credential"):
			stats.agentRevokes++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			stats.getCalls++
			if stats.states["target-spec-1"] == core.WorkOrderCompleted {
				if after == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_ = json.NewEncoder(w).Encode(after)
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order:      core.WorkOrder{ID: "target-spec-1", TaskID: "target", Stage: core.StageSpec, State: stats.states["target-spec-1"]},
				Task:       core.Task{ID: "target", Title: "Plan target", State: core.TaskRunning, Repo: "conveyor", BaseBranch: "main"},
				Repository: config.Repo{Name: "conveyor", URL: origin, Base: "main"}, Dispatch: "run", Auth: "user",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			stats.claimCalls++
			var claim struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			sessions["target-spec-1"] = claim.SessionID
			stats.states["target-spec-1"] = core.WorkOrderClaimed
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "target-spec-1", TaskID: "target", Stage: core.StageSpec, State: core.WorkOrderClaimed, AttemptID: "attempt-spec", LeaseExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reconcile"):
			state := stats.states["target-spec-1"]
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "target-spec-1", State: state}, Authorized: state == core.WorkOrderClaimed})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			stats.releaseCalls++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	command := fakeClaudeCommandYAML(t, "TestTaskRunHarnessHelper")
	localConfig := withHealthyRunProbe(t, strings.Replace(string(template), exampleHarnessCommand, command, 1))
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(configPath, []byte(localConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err = runTask(t.Context(), c, "target", configPath, strings.NewReader(input), &output, step, terminal)
	mu.Lock()
	defer mu.Unlock()
	return stats, output.String(), err
}

func TestRunTaskExecutesConfirmedSpecAndStopsAtOperatorGate(t *testing.T) {
	stats, output, err := runSpecTaskScenario(t, "yes\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.states["target-spec-1"] != core.WorkOrderCompleted || stats.getCalls != 2 || stats.claimCalls != 1 || stats.planSubmits != 1 || stats.releaseCalls != 0 || stats.agentIssues != 1 || stats.agentRevokes != 1 || countValues(stats.mcpCredentials, "Bearer child-agent-credential") != 2 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(stats.progress) != 1 || stats.progress[0] != "conveyor run mode: confirmed-per-stage" {
		t.Fatalf("progress=%q", stats.progress)
	}
	for _, want := range []string{"Next: spec work order target-spec-1", "Execution: harness local-agent, model gpt-5.6-sol, effort high, timeout 30m", "Proceed with spec?", "task target is waiting on spec approval gate; submitted execution plan v1; no further work order was claimed\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestAttachedRunApprovesFreshGateWithParentCredentialAndNoClaim(t *testing.T) {
	var reads, decisions, claims int
	resolved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer parent-user-credential" {
			http.Error(w, "wrong credential", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			reads++
			state := core.TaskAwaiting
			var gate *workerservice.TaskRunGate
			if resolved {
				state = core.TaskMerged
			} else {
				gate = &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan v3", SpecVersion: 3, CanOperate: true, CanRequestChanges: true}
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", Title: "Ship target", State: state}, Gate: gate, Dispatch: "run", Auth: "user"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/target/review":
			decisions++
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["action"] != "approve" || request["reason_code"] != "approved" {
				http.Error(w, "wrong decision", http.StatusBadRequest)
				return
			}
			resolved = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: "target", State: core.TaskMerged}})
		case strings.Contains(r.URL.Path, "/claim"):
			claims++
			http.Error(w, "must not claim while waiting", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	err := runTaskWithPresentation(t.Context(), c, "target", filepath.Join(t.TempDir(), "unused.yaml"), strings.NewReader("\nk\n"), &output, false, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || decisions != 1 || claims != 0 {
		t.Fatalf("reads=%d decisions=%d claims=%d", reads, decisions, claims)
	}
	// "Gate decision recorded" is a transient in-frame notice now; assert the
	// durable outcome lines instead.
	for _, want := range []string{"spec approval gate", "submitted execution plan v3", "No claim held", "finished in state merged"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q: %q", want, output.String())
		}
	}
}

func TestAttachedRunPreservesGateInputAcrossPoll(t *testing.T) {
	prior := runGatePollInterval
	runGatePollInterval = 5 * time.Millisecond
	defer func() { runGatePollInterval = prior }()

	var mutex sync.Mutex
	reads, decisions, claims := 0, 0, 0
	resolved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			reads++
			state := core.TaskAwaiting
			var gate *workerservice.TaskRunGate
			if resolved {
				state = core.TaskMerged
			} else {
				gate = &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "plan v1", CanOperate: true, CanRequestChanges: true}
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: state}, Gate: gate, Dispatch: "run", Auth: "user"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/target/review":
			decisions++
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["action"] != "approve" {
				http.Error(w, "wrong decision", http.StatusBadRequest)
				return
			}
			resolved = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: "target", State: core.TaskMerged}})
		case strings.Contains(r.URL.Path, "/claim"):
			claims++
			http.Error(w, "must not claim while waiting", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	input, writer := io.Pipe()
	t.Cleanup(func() {
		_ = input.Close()
		_ = writer.Close()
	})
	go func() {
		_, _ = io.WriteString(writer, "\nk")
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(writer, "\n")
	}()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", input, &output, false, true, true, false); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if reads < 3 || decisions != 1 || claims != 0 {
		t.Fatalf("reads=%d decisions=%d claims=%d output=%q", reads, decisions, claims, output.String())
	}
}

func TestAttachedRawRunKeepsLegacyGatePromptPath(t *testing.T) {
	resolved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && !resolved:
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Task: core.Task{ID: "target", Title: "Ship target", State: core.TaskAwaiting},
				Gate: &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "plan v1", CanOperate: true},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskMerged}})
		case r.Method == http.MethodPost:
			resolved = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: "target", State: core.TaskMerged}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("approve\n"), &output, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Gate action [approve/changes/wait]:") || strings.Contains(output.String(), "\x1b[?25l") {
		t.Fatalf("raw gate path unexpectedly used the Bubble Tea renderer: %q", output.String())
	}
}

func TestAttachedRunRequestsMergeGateChangesWithFeedback(t *testing.T) {
	resolved := false
	feedback := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			if resolved {
				_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskClosed}})
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskAwaiting}, Gate: &workerservice.TaskRunGate{Kind: "merge", Label: "merge approval gate", Summary: "branch into main", CanRequestChanges: true}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/request-changes"):
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			feedback = request["feedback"]
			resolved = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: "target", State: core.TaskQueued}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("\n  fix the race  \n"), &output, true, true, true, false); err != nil {
		t.Fatal(err)
	}
	if feedback != "fix the race" || !strings.Contains(output.String(), "merge approval gate") {
		t.Fatalf("feedback=%q output=%q", feedback, output.String())
	}
}

func TestAttachedRunAutoNeverApprovesGate(t *testing.T) {
	decisions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Task: core.Task{ID: "target", Title: "Ship target", State: core.TaskAwaiting},
				Gate: &workerservice.TaskRunGate{Kind: "merge", Label: "merge approval gate", Summary: "task branch", CanOperate: true},
			})
			return
		}
		decisions++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", strings.NewReader(""), &output, false, true, true, false); err != nil {
		t.Fatal(err)
	}
	if decisions != 0 || !strings.Contains(output.String(), "merge approval gate") || !strings.Contains(output.String(), "Ran: none") {
		t.Fatalf("decisions=%d output=%q", decisions, output.String())
	}
}

func TestAttachedRunPollsGateResolvedElsewhereWithoutClaim(t *testing.T) {
	prior := runGatePollInterval
	runGatePollInterval = 5 * time.Millisecond
	defer func() { runGatePollInterval = prior }()
	reads, mutations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
			w.WriteHeader(http.StatusAccepted)
			return
		}
		reads++
		if reads == 1 {
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskAwaiting}, Gate: &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "plan v1", CanOperate: true}})
			return
		}
		_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskMerged}})
	}))
	defer server.Close()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", input, &output, false, true, true, false); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || mutations != 0 || !strings.Contains(output.String(), "finished in state merged") {
		t.Fatalf("reads=%d mutations=%d output=%q", reads, mutations, output.String())
	}
}

func TestAdaptiveTaskRunPollingBacksOffAndObservesTransitionWithinBound(t *testing.T) {
	priorInterval, priorJitter := runGatePollInterval, runPollJitter
	runGatePollInterval = 5 * time.Millisecond
	runPollJitter = func(delay, _ time.Duration) time.Duration { return delay }
	defer func() {
		runGatePollInterval = priorInterval
		runPollJitter = priorJitter
	}()

	var mutex sync.Mutex
	var requests []time.Time
	var changedAt time.Time
	proposal := workerservice.TaskRunProposal{Kind: "design", DocumentID: "adaptive", Version: 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		requests = append(requests, time.Now())
		count := len(requests)
		if count == 3 {
			changedAt = time.Now()
		}
		changed := count >= 4
		mutex.Unlock()
		item := workerservice.DispatchOrder{Task: core.Task{ID: "target"}}
		if changed {
			item.PendingProposals = []workerservice.TaskRunProposal{proposal}
		}
		_ = json.NewEncoder(w).Encode(item)
	}))
	defer server.Close()

	appeared := make(chan struct{})
	var once sync.Once
	presentation := taskProposalPresentation{
		actions: make(chan runTUIAction),
		update: func(items []workerservice.TaskRunProposal) {
			if len(items) != 0 {
				once.Do(func() { close(appeared) })
			}
		},
		notice: func(string) {},
	}
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := runStageWithTaskProposalPresentation(ctx, cancel, c, c.token, "target", nil, presentation, func() error {
		select {
		case <-appeared:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(requests) != 4 {
		t.Fatalf("requests=%d times=%v", len(requests), requests)
	}
	intervals := []time.Duration{requests[1].Sub(requests[0]), requests[2].Sub(requests[1]), requests[3].Sub(requests[2])}
	if intervals[1] < intervals[0]+3*time.Millisecond || intervals[2] < intervals[1]+7*time.Millisecond {
		t.Fatalf("unchanged polls did not back off: %v", intervals)
	}
	if latency := requests[3].Sub(changedAt); latency > runGatePollInterval*8+10*time.Millisecond {
		t.Fatalf("transition latency=%s exceeds documented bound=%s", latency, runGatePollInterval*8)
	}
}

func TestEightIdleLauncherPollingRequestBenchmark(t *testing.T) {
	const (
		launchers      = 8
		adaptiveBase   = 10 * time.Millisecond
		legacyInterval = adaptiveBase * 8
		idleInterval   = 190 * time.Millisecond
	)
	priorInterval, priorJitter := runGatePollInterval, runPollJitter
	runGatePollInterval = adaptiveBase
	runPollJitter = func(delay, _ time.Duration) time.Duration { return delay }
	defer func() {
		runGatePollInterval = priorInterval
		runPollJitter = priorJitter
	}()

	measure := func(t *testing.T, adaptive bool) int {
		t.Helper()
		var mutex sync.Mutex
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mutex.Lock()
			requests++
			mutex.Unlock()
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "idle"}})
		}))
		defer server.Close()

		start := make(chan struct{})
		var ready, done sync.WaitGroup
		ready.Add(launchers)
		done.Add(launchers)
		errs := make(chan error, launchers)
		var idleCtx context.Context
		for index := 0; index < launchers; index++ {
			go func(taskID string) {
				defer done.Done()
				ready.Done()
				<-start
				c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
				if _, err := c.getTaskRunOrderContext(idleCtx, c.token, taskID); err != nil {
					errs <- err
					return
				}
				if !adaptive {
					ticker := time.NewTicker(legacyInterval)
					defer ticker.Stop()
					for {
						select {
						case <-idleCtx.Done():
							return
						case <-ticker.C:
							if _, err := c.getTaskRunOrderContext(idleCtx, c.token, taskID); err != nil && !errors.Is(err, context.DeadlineExceeded) {
								errs <- err
								return
							}
						}
					}
				}

				stageCtx, cancelStage := context.WithCancel(idleCtx)
				err := runStageWithTaskProposalPresentation(stageCtx, cancelStage, c, c.token, taskID, nil, taskProposalPresentation{
					actions: make(chan runTUIAction), update: func([]workerservice.TaskRunProposal) {}, notice: func(string) {},
				}, func() error {
					<-stageCtx.Done()
					return stageCtx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) {
					errs <- err
				}
			}(fmt.Sprintf("idle-%d", index))
		}
		ready.Wait()
		var cancel context.CancelFunc
		idleCtx, cancel = context.WithTimeout(t.Context(), idleInterval)
		close(start)
		done.Wait()
		cancel()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		mutex.Lock()
		defer mutex.Unlock()
		return requests
	}

	before := measure(t, false)
	after := measure(t, true)
	if before != 24 || after != 40 {
		t.Fatalf("requests_before=%d requests_after=%d, want 24 and 40", before, after)
	}
	t.Logf("eight_idle_launcher_polling interval=%s concurrency=%d startup=immediate_request warmup=none jitter=disabled_for_reproducibility timing_scale=1/25 legacy_fixed_interval=%s adaptive_base=%s adaptive_max=%s requests_before=%d requests_after=%d observation=request_increase", idleInterval, launchers, legacyInterval, adaptiveBase, adaptiveBase*8, before, after)
}

func TestStageProposalPollingReturnsAuthenticationFailure(t *testing.T) {
	priorInterval, priorJitter := runGatePollInterval, runPollJitter
	runGatePollInterval = 5 * time.Millisecond
	runPollJitter = func(delay, _ time.Duration) time.Duration { return delay }
	defer func() {
		runGatePollInterval = priorInterval
		runPollJitter = priorJitter
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "expired", workspace: "demo"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	childStarted := make(chan struct{})
	childCleanedUp := make(chan struct{})
	err := runStageWithTaskProposalPresentation(ctx, cancel, c, c.token, "target", nil, taskProposalPresentation{
		actions: make(chan runTUIAction), update: func([]workerservice.TaskRunProposal) {}, notice: func(string) {},
	}, func() error {
		close(childStarted)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(childCleanedUp)
		return ctx.Err()
	})
	var response *workerHTTPError
	if !errors.As(err, &response) || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error=%v response=%+v", err, response)
	}
	select {
	case <-childStarted:
	default:
		t.Fatal("stage child did not start")
	}
	select {
	case <-childCleanedUp:
	default:
		t.Fatal("authentication failure returned before the stage child completed cleanup")
	}
}

func TestAttachedRunGateConflictRefreshesRecordedState(t *testing.T) {
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads++
			if reads == 1 {
				_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskAwaiting}, Gate: &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "plan v1", CanOperate: true}})
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskClosed}})
			return
		}
		http.Error(w, "gate already resolved", http.StatusConflict)
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("\nk\n"), &output, true, true, true, false); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || !strings.Contains(output.String(), "finished in state closed") {
		t.Fatalf("reads=%d output=%q", reads, output.String())
	}
}

func TestTaskRunProposalClientUsesExistingAuthenticatedEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer parent-user-credential" {
			http.Error(w, "wrong credential", http.StatusUnauthorized)
			return
		}
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/review") {
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["action"] != string(core.InterventionRedirect) || request["reason_code"] != "plan-revision-approved" {
				http.Error(w, "wrong plan revision action", http.StatusBadRequest)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	for _, proposal := range []workerservice.TaskRunProposal{
		{Kind: "requirement", DocumentID: "req-run", Version: 2},
		{Kind: "design", DocumentID: "design-run", Version: 7},
		{Kind: "decision", DocumentID: "DEC-9", Version: 1},
		{Kind: "plan_revision", DocumentID: "target", Version: 3},
	} {
		if err := c.confirmTaskRunProposalContext(t.Context(), c.token, "target", proposal); err != nil {
			t.Fatalf("confirm %+v: %v", proposal, err)
		}
	}
	want := []string{
		"/v1/requirements/req-run/versions/2/confirm",
		"/v1/system-designs/design-run/versions/7/confirm",
		"/v1/decisions/DEC-9/confirm",
		"/v1/tasks/target/review",
	}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths=%q want=%q", paths, want)
	}
}

func TestStageProposalPollingSurfacesAndRefreshesConfirmationRaces(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "confirmed"
		if conflict {
			name = "resolved concurrently"
		}
		t.Run(name, func(t *testing.T) {
			prior := runGatePollInterval
			runGatePollInterval = 5 * time.Millisecond
			defer func() { runGatePollInterval = prior }()
			proposal := workerservice.TaskRunProposal{Kind: "design", DocumentID: "design-run", Title: "Attached run", Version: 4, CanConfirm: true}
			var mutex sync.Mutex
			reads, confirmations := 0, 0
			resolved := false
			finished := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer parent-user-credential" {
					http.Error(w, "wrong credential", http.StatusUnauthorized)
					return
				}
				mutex.Lock()
				defer mutex.Unlock()
				switch r.Method {
				case http.MethodGet:
					reads++
					pending := []workerservice.TaskRunProposal(nil)
					if reads >= 2 && !resolved {
						pending = []workerservice.TaskRunProposal{proposal}
					}
					_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{Task: core.Task{ID: "target"}, PendingProposals: pending})
				case http.MethodPost:
					confirmations++
					resolved = true
					close(finished)
					if conflict {
						http.Error(w, "already resolved", http.StatusConflict)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			actions := make(chan runTUIAction, 1)
			appeared := make(chan struct{})
			var appearedOnce sync.Once
			var snapshots [][]workerservice.TaskRunProposal
			var notices []string
			presentation := taskProposalPresentation{
				actions: actions,
				update: func(next []workerservice.TaskRunProposal) {
					mutex.Lock()
					snapshots = append(snapshots, append([]workerservice.TaskRunProposal(nil), next...))
					mutex.Unlock()
					if len(next) > 0 {
						appearedOnce.Do(func() { close(appeared) })
					}
				},
				notice: func(message string) {
					mutex.Lock()
					notices = append(notices, message)
					mutex.Unlock()
				},
			}
			go func() {
				<-appeared
				actions <- runTUIAction{decision: runConfirmProposal, proposal: &proposal}
			}()
			c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := runStageWithTaskProposalPresentation(ctx, cancel, c, c.token, "target", nil, presentation, func() error {
				select {
				case <-finished:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if reads < 2 || confirmations != 1 {
				t.Fatalf("reads=%d confirmations=%d", reads, confirmations)
			}
			var sawPending, sawCleared bool
			for _, snapshot := range snapshots {
				sawPending = sawPending || len(snapshot) == 1
				sawCleared = sawCleared || (sawPending && len(snapshot) == 0)
			}
			if !sawPending || !sawCleared {
				t.Fatalf("snapshots=%+v", snapshots)
			}
			joined := strings.Join(notices, "\n")
			if conflict && !strings.Contains(joined, "state changed") {
				t.Fatalf("race notice missing: %q", joined)
			}
			if !conflict && !strings.Contains(joined, "Confirmed design design-run v4") {
				t.Fatalf("confirmation notice missing: %q", joined)
			}
		})
	}
}

func TestRunTaskDeclinesSpecBeforeClaim(t *testing.T) {
	stats, output, err := runSpecTaskScenario(t, "no\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.claimCalls != 0 || stats.planSubmits != 0 || stats.releaseCalls != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if !strings.Contains(output, "Ran: none") || !strings.Contains(output, "Resume: conveyor run target") {
		t.Fatalf("output=%q", output)
	}
}

func TestRunTaskExecutesConfirmedImplementReviewChain(t *testing.T) {
	stats, output, err := runTaskScenario(t, "yes\nyes\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.states["target-implement-1"] != core.WorkOrderSubmitted || stats.states["target-review-1"] != core.WorkOrderCompleted || stats.getCalls != 3 || stats.claimCalls != 2 || stats.reviewSubmits != 1 || stats.verdictSubmits != 1 || stats.releaseCalls != 0 || stats.agentIssues != 2 || stats.agentRevokes != 2 || stats.cleanupChecks != 1 || stats.cleanupRecords != 1 || stats.localCleanups != 1 || countValues(stats.mcpCredentials, "Bearer child-agent-credential") != 4 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(stats.progress) != 2 || stats.progress[0] != "conveyor run mode: confirmed-per-stage" || stats.progress[1] != "conveyor run mode: confirmed-per-stage" {
		t.Fatalf("progress=%q", stats.progress)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: implement work order target-implement-1", "Proceed with implement?", "Next: review work order target-review-1", "Proceed with review?", "Task target finished in state merged."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func countValues(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func TestRunTaskAdvancesAfterSubmittedChildrenLinger(t *testing.T) {
	previousRenew := workerClaimRenewInterval
	previousTerminalGrace := workerRunTerminalChildGrace
	previousTerminationGrace := workerProcessGroupTerminationGrace
	workerClaimRenewInterval = 25 * time.Millisecond
	workerRunTerminalChildGrace = 50 * time.Millisecond
	workerProcessGroupTerminationGrace = 100 * time.Millisecond
	t.Setenv("CONVEYOR_FAKE_TASK_RUN_LINGER_AFTER_SUBMIT", "1")
	t.Cleanup(func() {
		workerClaimRenewInterval = previousRenew
		workerRunTerminalChildGrace = previousTerminalGrace
		workerProcessGroupTerminationGrace = previousTerminationGrace
	})

	stats, output, err := runTaskScenario(t, "yes\nyes\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.reviewSubmits != 1 || stats.verdictSubmits != 1 || stats.claimCalls != 2 {
		t.Fatalf("run did not advance through the review stage: stats=%+v", stats)
	}
	if !strings.Contains(output, "work order is submitted; ending lingering implement session") || !strings.Contains(output, "work order is completed; ending lingering review session") || !strings.Contains(output, "Next: review work order target-review-1") {
		t.Fatalf("lingering-child reaping was not presented before stage advance: %q", output)
	}
}

func TestRunTaskDeclinesBeforeFirstClaim(t *testing.T) {
	stats, output, err := runTaskScenario(t, "no\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.claimCalls != 0 || stats.releaseCalls != 0 || len(stats.progress) != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	for _, want := range []string{"Ran: none", "Task target is currently running.", "Resume: conveyor run target"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestRunTaskDeclinesLaterStageWithoutClaimingIt(t *testing.T) {
	stats, output, err := runTaskScenario(t, "yes\nno\n", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.states["target-implement-1"] != core.WorkOrderSubmitted || stats.states["target-review-1"] != core.WorkOrderQueued || stats.claimCalls != 1 || stats.reviewSubmits != 1 || stats.verdictSubmits != 0 || stats.releaseCalls != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if !strings.Contains(output, "Ran: implement") || !strings.Contains(output, "Resume: conveyor run target") {
		t.Fatalf("output=%q", output)
	}
}

func TestRunTaskDefaultChainsAndRecordsMode(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			stats, output, err := runTaskScenario(t, "", false, terminal)
			if err != nil {
				t.Fatal(err)
			}
			if stats.claimCalls != 2 || stats.reviewSubmits != 1 || stats.verdictSubmits != 1 || stats.releaseCalls != 0 || len(stats.progress) != 2 {
				t.Fatalf("stats=%+v", stats)
			}
			for _, progress := range stats.progress {
				if progress != "conveyor run mode: auto-chained" {
					t.Fatalf("progress=%q", stats.progress)
				}
			}
			if strings.Contains(output, "Proceed with") {
				t.Fatalf("default output prompted: %q", output)
			}
		})
	}
}

func TestRunTaskNonTerminalStepPresentsAndDoesNotClaim(t *testing.T) {
	stats, output, err := runTaskScenario(t, "", true, false)
	if err == nil || !strings.Contains(err.Error(), "--step") {
		t.Fatalf("err=%v", err)
	}
	if stats.claimCalls != 0 || stats.releaseCalls != 0 || len(stats.progress) != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: implement work order target-implement-1", "No work order was claimed", "--step", "Drop --step or attach a terminal"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestRunTaskMissingSetupPresentsPendingOrderWithoutClaiming(t *testing.T) {
	claimCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order: core.WorkOrder{ID: "target-implement-1", TaskID: "target", Stage: core.StageImplement, State: core.WorkOrderQueued},
				Task:  core.Task{ID: "target", Title: "Ship target", State: core.TaskRunning},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			claimCalls++
			http.Error(w, "must not claim", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err := runTask(t.Context(), c, "target", filepath.Join(t.TempDir(), "missing.yaml"), strings.NewReader(""), &output, false, false)
	if err == nil || !strings.Contains(err.Error(), "load local execution config") || !strings.Contains(err.Error(), "missing.yaml") || !strings.Contains(err.Error(), "conveyor config init-execution") {
		t.Fatalf("err=%v", err)
	}
	if claimCalls != 0 {
		t.Fatalf("claim calls=%d", claimCalls)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: implement work order target-implement-1"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q: %q", want, output.String())
		}
	}
}

func runTaskSelectionErrorScenario(t *testing.T, stage core.Stage, reviewSeat int, mutateConfig func(string) string) (int, string, error) {
	t.Helper()
	claimCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order: core.WorkOrder{ID: "target-" + string(stage) + "-1", TaskID: "target", Stage: stage, State: core.WorkOrderQueued, ReviewSeat: reviewSeat},
				Task:  core.Task{ID: "target", Title: "Ship target", State: core.TaskRunning},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			claimCalls++
			http.Error(w, "must not claim", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(configPath, []byte(mutateConfig(string(template))), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err = runTask(t.Context(), c, "target", configPath, strings.NewReader(""), &output, false, false)
	return claimCalls, output.String(), err
}

func TestRunTaskNoMCPRoutePresentsPendingOrderWithoutClaiming(t *testing.T) {
	claimCalls, output, err := runTaskSelectionErrorScenario(t, core.StageReview, 1, func(value string) string {
		value = strings.Replace(value, "      review:\n        execution: mcp\n        timeout: 1h", "      review:\n        execution: in_process\n        timeout: 1h", 1)
		value = strings.Replace(value, "    review:\n      seats:\n        - {model: gpt-5.6, harness: local-agent}\n        - {model: claude-opus-4.1, harness: local-agent}", "    review:\n      seats:\n        - {model: gpt-5.6}", 1)
		return value
	})
	if err == nil || !strings.Contains(err.Error(), "no MCP route for review") || !strings.Contains(err.Error(), localExecutionSetupCommand) {
		t.Fatalf("err=%v", err)
	}
	if claimCalls != 0 {
		t.Fatalf("claim calls=%d", claimCalls)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: review work order target-review-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestRunTaskInvalidSpecHarnessPresentsPendingOrderWithoutClaiming(t *testing.T) {
	claimCalls, output, err := runTaskSelectionErrorScenario(t, core.StageSpec, 0, func(value string) string {
		return strings.Replace(value, "      spec:\n        harness: local-agent", "      spec:\n        harness: missing-spec-harness", 1)
	})
	if err == nil || !strings.Contains(err.Error(), "stage spec") || !strings.Contains(err.Error(), `unknown harness "missing-spec-harness"`) || !strings.Contains(err.Error(), localExecutionSetupCommand) {
		t.Fatalf("err=%v", err)
	}
	if claimCalls != 0 {
		t.Fatalf("claim calls=%d", claimCalls)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: spec work order target-spec-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestSelectLocalRunDispatchNamesMissingSpecRoute(t *testing.T) {
	item := workerservice.DispatchOrder{Order: core.WorkOrder{Stage: core.StageSpec}}
	_, err := selectLocalRunDispatch(item, &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{}}})
	if err == nil || err.Error() != "local execution config has no MCP route for spec" {
		t.Fatalf("err=%v", err)
	}
}

func TestRunTaskReviewSeatOverflowPresentsShortfallWithoutClaiming(t *testing.T) {
	claimCalls, output, err := runTaskSelectionErrorScenario(t, core.StageReview, 3, func(value string) string { return value })
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if claimCalls != 0 {
		t.Fatalf("claim calls=%d", claimCalls)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: review work order target-review-1", "requires seat 3", "configures 2 seat(s)", "left queued", "nothing was claimed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestTaskRunHarnessHelper(t *testing.T) {
	if os.Getenv("CONVEYOR_FAKE_TASK_RUN_HARNESS") != "1" {
		return
	}
	sessionID := os.Getenv("CONVEYOR_SESSION_ID")
	var configPath string
	for _, arg := range os.Args {
		if filepath.Base(arg) == "mcp.json" {
			configPath = arg
			break
		}
	}
	if configPath == "" {
		t.Fatal("MCP config argument not found")
	}
	emitFakeClaudeReceipt("", "")
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
	call := func(name string) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{
			"workspace_id": os.Getenv("CONVEYOR_WORKSPACE"), "work_order_id": os.Getenv("CONVEYOR_WORK_ORDER_ID"), "session_id": sessionID,
		}}})
		server := document.Servers["conveyor"]
		request, requestErr := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", os.Expand(server.Headers["Authorization"], os.Getenv))
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %s", name, response.Status)
		}
	}
	call("get_work_order")
	if strings.Contains(os.Getenv("CONVEYOR_WORK_ORDER_ID"), "spec") {
		head, readErr := os.ReadFile(filepath.Join(".git", "HEAD"))
		if readErr != nil || strings.HasPrefix(string(head), "ref:") {
			t.Fatalf("spec checkout is not detached: head=%q err=%v", head, readErr)
		}
		gitConfig, readErr := os.ReadFile(filepath.Join(".git", "config"))
		if readErr != nil || !strings.Contains(string(gitConfig), "disabled://conveyor-spec-read-only") {
			t.Fatalf("spec checkout push remote is not disabled: err=%v", readErr)
		}
		info, statErr := os.Stat("README.md")
		if statErr != nil {
			t.Fatalf("stat spec checkout: %v", statErr)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("spec checkout is not read-only: mode=%v", info.Mode())
		}
		call("submit_plan")
	} else if strings.Contains(os.Getenv("CONVEYOR_WORK_ORDER_ID"), "implement") {
		call("submit_for_review")
	} else {
		call("submit_review_verdict")
	}
	if os.Getenv("CONVEYOR_FAKE_TASK_RUN_LINGER_AFTER_SUBMIT") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func TestRunCmdDefaultAndHiddenAutoChainIdentically(t *testing.T) {
	for _, flags := range [][]string{{}, {"--auto"}, {"--auto=false"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			stats, output, err := runTaskScenario(t, "", false, false, flags)
			if err != nil {
				t.Fatal(err)
			}
			if stats.claimCalls != 2 || stats.reviewSubmits != 1 || stats.verdictSubmits != 1 || stats.agentRevokes != 2 || len(stats.progress) != 2 {
				t.Fatalf("stats=%+v", stats)
			}
			for _, progress := range stats.progress {
				if progress != "conveyor run mode: auto-chained" {
					t.Fatalf("progress=%q", stats.progress)
				}
			}
			wantNotice := ""
			if len(flags) > 0 {
				wantNotice = "--auto is now the default and will be removed in a later release\n"
			}
			if stats.stderr != wantNotice || strings.Contains(output, "Proceed with") || strings.Contains(output, "--auto is now") {
				t.Fatalf("stdout=%q stderr=%q", output, stats.stderr)
			}
		})
	}
}

func TestRunCmdAutoIsHiddenFromHelp(t *testing.T) {
	command := runCmd()
	command.SetArgs([]string{"--help"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "--auto") || !strings.Contains(output.String(), "--step") || !strings.Contains(output.String(), "confirm each stage before it is claimed") {
		t.Fatalf("help=%q", output.String())
	}
}

func TestRunCmdConflictingFlagsFailBeforeAnyRequest(t *testing.T) {
	for _, flags := range [][]string{{"--auto", "--step"}, {"--step", "--auto"}, {"--auto=false", "--step"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			stats, _, err := runTaskScenario(t, "", false, false, flags)
			if err == nil || !strings.Contains(err.Error(), "--auto") || !strings.Contains(err.Error(), "--step") {
				t.Fatalf("err=%v", err)
			}
			if stats.requests != 0 || stats.claimCalls != 0 || stats.agentIssues != 0 || stats.stderr != "" {
				t.Fatalf("stats=%+v", stats)
			}
		})
	}
}

func TestRunCmdNonTerminalStepClaimsNothing(t *testing.T) {
	stats, output, err := runTaskScenario(t, "", true, false, []string{"--step"})
	if err == nil || !strings.Contains(err.Error(), "--step") || stats.claimCalls != 0 || stats.agentIssues != 0 || len(stats.progress) != 0 {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
	if !strings.Contains(output, "Next: implement work order target-implement-1") || !strings.Contains(output, "Drop --step or attach a terminal") {
		t.Fatalf("output=%q", output)
	}
}

func TestRunTaskDefaultNonTerminalStopsAtPlanGateWithoutClaim(t *testing.T) {
	stats, output, err := runSpecTaskScenario(t, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.claimCalls != 1 || stats.planSubmits != 1 || stats.states["target-spec-1"] != core.WorkOrderCompleted || stats.agentRevokes != 1 || stats.releaseCalls != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(stats.progress) != 1 || stats.progress[0] != "conveyor run mode: auto-chained" {
		t.Fatalf("progress=%q", stats.progress)
	}
	if !strings.Contains(output, "task target is waiting on spec approval gate; submitted execution plan v1; no further work order was claimed\n") || strings.Contains(output, "Proceed with") {
		t.Fatalf("output=%q", output)
	}
}

func TestTaskRunClaimPreservesTypedCodeAndConciseDiagnostic(t *testing.T) {
	for _, code := range []string{"review_awaiting_proposal", "unknown_refusal", ""} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Conveyor-Error-Code", code)
				http.Error(w, "claim refused", http.StatusConflict)
			}))
			defer server.Close()
			c := &client{base: server.URL}
			_, err := c.claimTaskRunOrderContext(t.Context(), "parent-token", workerservice.DispatchOrder{Task: core.Task{ID: "target"}, Order: core.WorkOrder{ID: "review"}}, "session", "secret")
			var response *workerHTTPError
			if !errors.As(err, &response) || response.Code != code || response.StatusCode != http.StatusConflict || err.Error() != "claim refused" {
				t.Fatalf("error=%v response=%+v", err, response)
			}
		})
	}
}

func TestTaskRunReviewClaimRefusalRefreshesAndWaitsOnlyForProposalCode(t *testing.T) {
	for _, code := range []string{"review_awaiting_proposal", "other_conflict", ""} {
		t.Run(code, func(t *testing.T) {
			previousDirectory, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			fixture := newGitFixture(t)
			t.Chdir(previousDirectory)
			t.Setenv(localGitTokenEnv, "")
			template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
			if err = os.WriteFile(configPath, []byte(withHealthyRunProbe(t, string(template))), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(fixture.primary)
			prior := runGatePollInterval
			runGatePollInterval = 5 * time.Millisecond
			defer func() { runGatePollInterval = prior }()
			reads, claims, childCalls := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer parent-token" {
					t.Error("wrong credential")
				}
				switch {
				case r.URL.Path == "/v1/forge-token":
					_, _ = io.WriteString(w, `{"configured":true}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/run-order"):
					reads++
					item := workerservice.DispatchOrder{
						Order:      core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewSeat: 1},
						Task:       core.Task{ID: "target", State: core.TaskRunning, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"},
						Repository: config.Repo{Name: "conveyor", URL: fixture.origin, Base: "main"}, Dispatch: "run", Auth: "user",
					}
					// The first read is stale; a later operator act resolves the
					// proposal only after two reads prove we waited unclaimed.
					if reads == 2 || reads == 3 {
						if claims != 1 {
							t.Errorf("claimed while proposal pending: %d", claims)
						}
						item.PendingProposals = []workerservice.TaskRunProposal{{Kind: "requirement", DocumentID: "req-run", Title: "Run", Version: 2, ActorHint: "an operator can confirm"}}
					}
					_ = json.NewEncoder(w).Encode(item)
				case strings.HasSuffix(r.URL.Path, "/claim"):
					claims++
					if claims == 1 {
						w.Header().Set("X-Conveyor-Error-Code", code)
						http.Error(w, "review waits for proposal", http.StatusConflict)
					} else {
						http.Error(w, "review already claimed elsewhere", http.StatusConflict)
					}
				default:
					childCalls++
					http.Error(w, "unexpected child lifecycle call", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			c := &client{base: server.URL, token: "parent-token", workspace: "demo"}
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err = runTaskWithPresentation(ctx, c, "target", configPath, strings.NewReader(""), &output, false, true, true, true)
			if code == "review_awaiting_proposal" {
				if err == nil || err.Error() != "review already claimed elsewhere" || reads != 4 || claims != 2 || !strings.Contains(output.String(), "requirement proposal req-run v2") {
					t.Fatalf("reads=%d claims=%d error=%v output=%s", reads, claims, err, output.String())
				}
			} else if err == nil || err.Error() != "review waits for proposal" || reads != 1 || claims != 1 {
				t.Fatalf("unrelated conflict retried: reads=%d claims=%d error=%v", reads, claims, err)
			}
			if childCalls != 0 {
				t.Fatalf("unclaimed child calls=%d", childCalls)
			}
		})
	}
}

func TestAttachedRunConfirmsRequirementWhileReviewRemainsQueued(t *testing.T) {
	prior := runGatePollInterval
	runGatePollInterval = 5 * time.Millisecond
	defer func() { runGatePollInterval = prior }()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var mu sync.Mutex
	reads, confirmations, claims := 0, 0, 0
	resolved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer parent-user-credential" {
			t.Error("wrong credential")
		}
		switch {
		case r.URL.Path == "/v1/forge-token":
			_, _ = io.WriteString(w, `{"configured":true}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/run-order"):
			reads++
			item := workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskRunning}, Order: core.WorkOrder{ID: "target-review-1", Stage: core.StageReview, State: core.WorkOrderQueued}}
			if resolved {
				item.Task.State = core.TaskClosed
				item.Order = core.WorkOrder{}
			} else {
				item.PendingProposals = []workerservice.TaskRunProposal{{Kind: "requirement", DocumentID: "req-run", Title: "Attached run", Version: 2, CanConfirm: true, ActorHint: "an operator can confirm"}}
			}
			if reads == 3 {
				// Confirm only after two pending-state polls, so a queued order
				// cannot make the wait surface discard or restart the action.
				go func() { _, _ = io.WriteString(writer, "\nk\n") }()
			}
			_ = json.NewEncoder(w).Encode(item)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/requirements/req-run/versions/2/confirm":
			confirmations++
			resolved = true
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claims++
			http.Error(w, "must not claim pending review", http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", reader, &output, false, true, true, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads < 4 || confirmations != 1 || claims != 0 || !strings.Contains(output.String(), "Confirm requirement") || !strings.Contains(output.String(), "finished in state closed") {
		t.Fatalf("reads=%d confirmations=%d claims=%d output=%s", reads, confirmations, claims, output.String())
	}
}

func TestContextFreshnessSummaryDoesNotAcknowledgeOrPoll(t *testing.T) {
	f := core.ContextFreshness{SelectionRevision: strings.Repeat("a", 64), ObservationRevision: strings.Repeat("b", 64), ComparisonStatus: "changed", UnfetchedAdditions: 2, Snapshot: core.ContextSnapshot{OmittedCount: 3}, Deliveries: []core.ContextDelivery{{Failed: true}}}
	key, text := contextFreshnessSummary(f)
	again, repeated := contextFreshnessSummary(f)
	if key != again || text != repeated || !strings.Contains(text, "2 additions not fetched") || !strings.Contains(text, "1 fetch failures") || !strings.Contains(text, "3 omissions") {
		t.Fatal(text)
	}
	if f.AcknowledgementSupported || f.Deliveries[0].Fetched {
		t.Fatal("display changed delivery state")
	}
	f.Diagnostic = "observation_unavailable"
	f.Deliveries[0].Omitted = true
	f.Deliveries[0].Truncated = true
	f.Truncated = true
	changed, summary := contextFreshnessSummary(f)
	if changed == key || !strings.Contains(summary, "4 omissions, 1 truncated inputs") || !strings.Contains(summary, "incomplete coverage true") || !strings.Contains(summary, "observation_unavailable") {
		t.Fatal(summary)
	}
}

// exampleHarnessCommand is the Claude Code command line of the example
// harness, which end-to-end run tests replace with a fake claude child.
const exampleHarnessCommand = `command: [claude, -p, "{prompt}", --mcp-config, "{mcp_config}", --strict-mcp-config, --allowedTools, "mcp__conveyor__*", --output-format, stream-json, --verbose, --permission-mode, bypassPermissions, --add-dir, ..]`

// fakeClaudeCommandYAML renders fakeClaudeCommand as the example harness's
// YAML command line.
func fakeClaudeCommandYAML(t *testing.T, helper string) string {
	t.Helper()
	encoded, err := json.Marshal(fakeClaudeCommand(t, helper))
	if err != nil {
		t.Fatal(err)
	}
	return "command: " + string(encoded)
}

// withHealthyRunProbe replaces the example harness's claude probe with a
// present, healthy one: every run, with or without a named setup, probes its
// setup before the first claim (req-execution-configuration AC-10.4).
func withHealthyRunProbe(t *testing.T, value string) string {
	t.Helper()
	const probe = "probe_command: [claude, --version]"
	if !strings.Contains(value, probe) {
		t.Fatal("example harness probe fixture was not found")
	}
	return strings.Replace(value, probe, "probe_command: [echo, conveyor-probe-ok]", 1)
}

type unattachedRunCalls struct {
	reads, claims, renewals, other int
	paths                          []string
}

// runUnattachedProjection invokes a run without a terminal against one fixed
// live projection; nil serves an empty projection.
func runUnattachedProjection(t *testing.T, projection *workerservice.DispatchOrder, inputTerminal bool) (unattachedRunCalls, string, error) {
	t.Helper()
	var mu sync.Mutex
	var calls unattachedRunCalls
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls.paths = append(calls.paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			calls.reads++
			if projection == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(projection)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			calls.claims++
			http.Error(w, "must not claim", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/renew"):
			calls.renewals++
			http.Error(w, "must not renew", http.StatusInternalServerError)
		default:
			calls.other++
			http.Error(w, "must not mutate", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err := runTaskWithPresentation(ctx, c, "target", filepath.Join(t.TempDir(), "unused.yaml"), strings.NewReader(""), &output, false, inputTerminal, false, false)
	mu.Lock()
	defer mu.Unlock()
	return calls, output.String(), err
}

func TestUnattachedRunNamesLivePendingGate(t *testing.T) {
	task := core.Task{ID: "target", Title: "Ship target", State: core.TaskAwaiting, Branch: "conveyor/task-target", BaseBranch: "main", NextStage: core.StageReview}
	for _, gate := range []workerservice.TaskRunGate{
		{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan v3", SpecVersion: 3, CanOperate: true},
		{Kind: "merge", Label: "merge approval gate", Summary: "conveyor/task-target into main", CanOperate: true},
		{Kind: "plan_revision", Label: "plan revision gate", Summary: "implementation requested revision of execution plan v2", PlanVersion: 2},
		{Kind: "human", Label: "human recovery gate", Summary: "task is awaiting_human after review"},
	} {
		for _, inputTerminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/input-terminal=%t", gate.Kind, inputTerminal), func(t *testing.T) {
				gate := gate
				calls, output, err := runUnattachedProjection(t, &workerservice.DispatchOrder{Task: task, Gate: &gate, Dispatch: "run", Auth: "user"}, inputTerminal)
				if err != nil {
					t.Fatal(err)
				}
				want := "task target is waiting on " + gate.Label + "; " + gate.Summary + "; no work order was claimed\n"
				if output != want {
					t.Fatalf("output=%q want %q", output, want)
				}
				if calls.reads != 1 || calls.claims != 0 || calls.renewals != 0 || calls.other != 0 {
					t.Fatalf("unattached gate run made requests %v", calls.paths)
				}
			})
		}
	}
}

func TestUnattachedRunGateAndProposalsAreBothShown(t *testing.T) {
	proposals := []workerservice.TaskRunProposal{
		{Kind: "requirement", DocumentID: "req-local-task-runs", Title: "Runs", Version: 6, ActorHint: "an operator can confirm"},
		{Kind: "decision", DocumentID: "DEC-60", Title: "Probe", Version: 1, ActorHint: "an operator can confirm"},
	}
	task := core.Task{ID: "target", Title: "Ship target", State: core.TaskAwaiting}
	for name, projection := range map[string]workerservice.DispatchOrder{
		"gate": {Task: task, PendingProposals: proposals, Gate: &workerservice.TaskRunGate{Kind: "merge", Label: "merge approval gate", Summary: "conveyor/task-target into main"}},
		"queued review": {
			Order: core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewSeat: 1},
			Task:  task, PendingProposals: proposals,
			Gate: &workerservice.TaskRunGate{Kind: "human", Label: "human recovery gate", Summary: "task is awaiting_human after review"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls, output, err := runUnattachedProjection(t, &projection, false)
			if err != nil {
				t.Fatal(err)
			}
			wantLines := []string{
				"Waiting on requirement proposal req-local-task-runs v6; an operator can confirm",
				"Waiting on decision proposal DEC-60 v1; an operator can confirm",
				"task target is waiting on " + projection.Gate.Label + "; " + projection.Gate.Summary + "; no work order was claimed",
			}
			if output != strings.Join(wantLines, "\n")+"\n" {
				t.Fatalf("output=%q", output)
			}
			if calls.reads != 1 || calls.claims != 0 || calls.renewals != 0 || calls.other != 0 {
				t.Fatalf("unattached run made requests %v", calls.paths)
			}
		})
	}
}

func TestUnattachedRunDoesNotInferGateFromLastStage(t *testing.T) {
	const idle = "task target has no claimable spec, implement, or review order\n"
	t.Run("fresh idle projection", func(t *testing.T) {
		calls, output, err := runUnattachedProjection(t, &workerservice.DispatchOrder{Task: core.Task{ID: "target", State: core.TaskRunning}}, false)
		if err != nil || output != idle || calls.reads != 1 || calls.claims != 0 || calls.other != 0 {
			t.Fatalf("err=%v output=%q calls=%v", err, output, calls.paths)
		}
	})
	t.Run("fresh empty projection", func(t *testing.T) {
		calls, output, err := runUnattachedProjection(t, nil, false)
		if err != nil || output != idle || calls.reads != 1 || calls.claims != 0 || calls.other != 0 {
			t.Fatalf("err=%v output=%q calls=%v", err, output, calls.paths)
		}
	})
	t.Run("fresh parked task with gate", func(t *testing.T) {
		calls, output, err := runUnattachedProjection(t, &workerservice.DispatchOrder{
			Task: core.Task{ID: "target", Title: "Ship target", State: core.TaskParked},
			Gate: &workerservice.TaskRunGate{Kind: "human", Label: "human recovery gate", Summary: "task is parked after review"},
		}, false)
		if err != nil || strings.Contains(output, "waiting on") || strings.Contains(output, "no claimable") || calls.claims != 0 || calls.other != 0 {
			t.Fatalf("terminal handling was not authoritative: err=%v output=%q calls=%v", err, output, calls.paths)
		}
	})
	for name, after := range map[string]*workerservice.DispatchOrder{
		"cleared after spec": {Task: core.Task{ID: "target", Title: "Plan target", State: core.TaskRunning}, Dispatch: "run", Auth: "user"},
		"empty after spec":   nil,
		"closed after spec":  {Task: core.Task{ID: "target", Title: "Plan target", State: core.TaskClosed}, Dispatch: "run", Auth: "user"},
	} {
		t.Run(name, func(t *testing.T) {
			stats, output, err := runSpecTaskScenarioAfter(t, "", false, false, after)
			if err != nil {
				t.Fatal(err)
			}
			if stats.claimCalls != 1 || stats.planSubmits != 1 {
				t.Fatalf("stats=%+v", stats)
			}
			if strings.Contains(output, "spec approval gate") || strings.Contains(output, "waiting on") || strings.Contains(output, "operator approval is required") {
				t.Fatalf("run inferred a gate from its last stage: %q", output)
			}
			if after != nil && after.Task.State == core.TaskClosed {
				if strings.Contains(output, "no claimable") || !strings.Contains(output, "closed") {
					t.Fatalf("terminal projection output=%q", output)
				}
			} else if !strings.HasSuffix(output, idle) {
				t.Fatalf("cleared projection output=%q", output)
			}
		})
	}
}

const runProbeHealthyScript = "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$CONVEYOR_TEST_RUN_PROBE_LOG\"\nprintf 'probe ok %s\\n' \"$1\"\n"

// writeRunProbeSetupConfig writes the example configuration with distinct
// stage, seat, and named-setup harnesses. The default setup routes every
// stage through local-agent and its two seats through seat-a and seat-b; the
// named setup alt routes everything through alt-agent. probes overrides a
// harness's probe argv (YAML flow sequence) and timeouts its probe timeout;
// every other harness runs the healthy logging probe.
func writeRunProbeSetupConfig(t *testing.T, probes, timeouts map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	healthy := filepath.Join(directory, "probe-healthy")
	if err := os.WriteFile(healthy, []byte(runProbeHealthyScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONVEYOR_TEST_RUN_PROBE_LOG", filepath.Join(directory, "probes.log"))
	template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value := string(template)
	start := strings.Index(value, "harnesses:\n")
	end := strings.Index(value, "\n\n# One durable review order")
	if start < 0 || end < start {
		t.Fatal("example harness block was not found")
	}
	var harnesses strings.Builder
	harnesses.WriteString("harnesses:\n")
	for _, name := range []string{"local-agent", "seat-a", "seat-b", "alt-agent"} {
		probe, ok := probes[name]
		if !ok {
			probe = `["` + healthy + `", ` + name + `]`
		}
		timeout, ok := timeouts[name]
		if !ok {
			timeout = "10s"
		}
		fmt.Fprintf(&harnesses, "  - name: %s\n    mcp_transport: toml_override\n    command: [agent-cli, --prompt, \"{prompt}\", --mcp-config, \"{mcp_config}\"]\n    model_args: [--model, \"{model}\"]\n    effort_args:\n      high: [--effort, high]\n    probe_command: %s\n    probe_timeout: %s\n", name, probe, timeout)
	}
	value = value[:start] + strings.TrimSuffix(harnesses.String(), "\n") + value[end:]
	for _, replacement := range [][2]string{
		{"    - model: gpt-5.6\n      harness: local-agent\n    - model: claude-opus-4.1\n      harness: local-agent", "    - model: gpt-5.6\n      harness: seat-a\n    - model: claude-opus-4.1\n      harness: seat-b"},
		{"        - {model: gpt-5.6, harness: local-agent}\n        - {model: claude-opus-4.1, harness: local-agent}\n", "        - {model: gpt-5.6, harness: seat-a}\n        - {model: claude-opus-4.1, harness: seat-b}\n" +
			"  - name: alt\n    refresh_review: delta\n    execution_settings:\n      control_plane:\n        triage: {model: gpt-5.6-luna, timeout: 20m}\n" +
			"      spec:\n        harness: alt-agent\n        model: gpt-5.6-sol\n        model_policy: explicit\n        timeout: 30m\n" +
			"      implementation:\n        harness: alt-agent\n        model_policy: harness_default\n        timeout: 4h\n" +
			"      review:\n        execution: mcp\n        timeout: 1h\n    review:\n      seats:\n        - {model: gpt-5.6, harness: alt-agent}\n"},
	} {
		if !strings.Contains(value, replacement[0]) {
			t.Fatalf("example fixture %q was not found", replacement[0])
		}
		value = strings.Replace(value, replacement[0], replacement[1], 1)
	}
	path := filepath.Join(directory, "conveyor.yaml")
	if err = os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRunProbeLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("CONVEYOR_TEST_RUN_PROBE_LOG"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	slices.Sort(lines)
	return lines
}

type runProbeResult struct {
	claims        int
	probesAtClaim []string
	other         []string
	output        string
	err           error
}

// runProbeScenario runs one spec order with the given setup. The claim is
// refused so the run ends at its first claim; the result records which probes
// had already completed when that claim arrived.
func runProbeScenario(t *testing.T, ctx context.Context, configPath, setupName string, preflight func()) runProbeResult {
	t.Helper()
	t.Setenv(localGitTokenEnv, "")
	var mu sync.Mutex
	var result runProbeResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order:      core.WorkOrder{ID: "target-spec-1", TaskID: "target", Stage: core.StageSpec, State: core.WorkOrderQueued},
				Task:       core.Task{ID: "target", Title: "Plan target", State: core.TaskRunning, Repo: "conveyor", BaseBranch: "main"},
				Repository: config.Repo{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}, Dispatch: "run", Auth: "user",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/target/run-orders/target-spec-1/claim":
			result.claims++
			result.probesAtClaim = readRunProbeLog(t)
			http.Error(w, "claimed elsewhere", http.StatusConflict)
		default:
			result.other = append(result.other, r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "user-credential", workspace: "demo", gitPreflight: func(context.Context, workerservice.DispatchOrder, []string) error {
		if preflight != nil {
			preflight()
		}
		return nil
	}}
	var output bytes.Buffer
	err := runTaskWithPresentationAndSetup(ctx, c, "target", configPath, setupName, strings.NewReader(""), &output, false, false, false, false)
	mu.Lock()
	defer mu.Unlock()
	result.output, result.err = output.String(), err
	return result
}

func TestRunProbesDefaultAndNamedSetupBeforeFirstClaim(t *testing.T) {
	for _, test := range []struct {
		setup string
		want  []string
	}{
		{setup: "", want: []string{"local-agent", "seat-a", "seat-b"}},
		{setup: "alt", want: []string{"alt-agent"}},
	} {
		t.Run("setup="+test.setup, func(t *testing.T) {
			configPath := writeRunProbeSetupConfig(t, nil, nil)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			result := runProbeScenario(t, t.Context(), configPath, test.setup, func() {
				if probes := readRunProbeLog(t); len(probes) != 0 {
					t.Errorf("probes ran before the Git preflight: %v", probes)
				}
			})
			if result.err == nil || result.err.Error() != "claimed elsewhere" || result.claims != 1 || len(result.other) != 0 {
				t.Fatalf("err=%v claims=%d other=%v output=%q", result.err, result.claims, result.other, result.output)
			}
			// Each distinct stage and seat definition is probed exactly once,
			// and every probe completed before the first claim.
			if !slices.Equal(result.probesAtClaim, test.want) || !slices.Equal(readRunProbeLog(t), test.want) {
				t.Fatalf("probes at claim=%v after=%v want %v", result.probesAtClaim, readRunProbeLog(t), test.want)
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("probe or setup selection rewrote the persisted configuration")
			}
			if loaded, err := config.Load(configPath); err != nil || loaded.DefaultSetup != "default" {
				t.Fatalf("persisted default=%q err=%v", loaded.DefaultSetup, err)
			}
		})
	}
}

func TestRunFailedProbeClaimsNothingAndNamesRemedy(t *testing.T) {
	directory := t.TempDir()
	script := func(name, contents string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+contents), 0o700); err != nil {
			t.Fatal(err)
		}
		return `["` + path + `"]`
	}
	failures := []struct {
		name, probe, timeout, message string
		cancel                        bool
	}{
		{name: "missing binary", probe: `["` + filepath.Join(directory, "absent-probe") + `"]`, message: "absent-probe"},
		{name: "nonzero exit", probe: script("nonzero", "printf 'harness unauthenticated\\n'\nexit 3\n"), message: "harness unauthenticated"},
		{name: "timeout", probe: script("timeout", "exec sleep 30\n"), timeout: "100ms", message: "probe timed out after 100ms"},
		{name: "cancelled", cancel: true, message: "context canceled"},
		{name: "empty result", probe: script("empty", "exit 0\n"), message: "probe printed no identifying output"},
		{name: "malformed result", probe: script("blank", "printf '  \\n\\t\\n'\n"), message: "probe printed no identifying output"},
	}
	for _, failure := range failures {
		for _, setup := range []struct{ name, harness, first, label string }{
			{name: "", harness: "seat-b", first: "local-agent", label: `default setup "default"`},
			{name: "alt", harness: "alt-agent", first: "alt-agent", label: `setup "alt"`},
		} {
			t.Run(failure.name+"/setup="+setup.name, func(t *testing.T) {
				probes, timeouts := map[string]string{}, map[string]string{}
				if failure.probe != "" {
					probes[setup.harness] = failure.probe
				}
				if failure.timeout != "" {
					timeouts[setup.harness] = failure.timeout
				}
				configPath := writeRunProbeSetupConfig(t, probes, timeouts)
				before, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var hook func()
				harness := setup.harness
				if failure.cancel {
					// Cancellation fails every probe; the first in name order is named.
					hook, harness = cancel, setup.first
				}
				result := runProbeScenario(t, ctx, configPath, setup.name, hook)
				if result.claims != 0 || len(result.other) != 0 {
					t.Fatalf("failed probe claimed=%d other=%v", result.claims, result.other)
				}
				for _, want := range []string{setup.label, "failed pre-claim harness probe", `"` + harness + `"`, failure.message, configPath, localExecutionSetupCommand + " --config " + configPath} {
					if result.err == nil || !strings.Contains(result.err.Error(), want) {
						t.Fatalf("error %v does not name %q", result.err, want)
					}
				}
				if !strings.Contains(result.output, "Next: spec work order target-spec-1") {
					t.Fatalf("pending order was not presented: %q", result.output)
				}
				after, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("failed probe rewrote the persisted configuration")
				}
			})
		}
	}
}
