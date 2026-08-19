package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	states                                           map[string]core.WorkOrderState
	getCalls, claimCalls, planSubmits, reviewSubmits int
	verdictSubmits, releaseCalls                     int
	progress                                         []string
}

func runTaskScenario(t *testing.T, input string, auto, terminal bool) (taskRunStats, string, error) {
	t.Helper()
	t.Setenv("CONVEYOR_FAKE_TASK_RUN_HARNESS", "1")
	var mu sync.Mutex
	stats := taskRunStats{states: map[string]core.WorkOrderState{
		"target-implement-1": core.WorkOrderQueued,
		"target-review-1":    core.WorkOrderQueued,
	}}
	sessions := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-credential" {
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
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			stats.getCalls++
			order := core.WorkOrder{ID: "target-implement-1", TaskID: "target", Stage: core.StageImplement, State: stats.states["target-implement-1"]}
			if stats.states["target-implement-1"] == core.WorkOrderSubmitted {
				order = core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: stats.states["target-review-1"], ReviewSeat: 1}
			}
			if stats.states["target-review-1"] == core.WorkOrderCompleted {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order:    order,
				Task:     core.Task{ID: "target", Title: "Ship target", State: core.TaskRunning, Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"},
				Dispatch: "run", Auth: "user",
			})
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
	command := `command: ["` + strings.ReplaceAll(os.Args[0], `"`, `\"`) + `", "-test.run=TestTaskRunHarnessHelper", "--", "{prompt}", "{mcp_config}"]`
	localConfig := strings.Replace(string(template), `command: [agent-cli, --prompt, "{prompt}", --mcp-config, "{mcp_config}"]`, command, 1)
	localConfig = strings.ReplaceAll(localConfig, "effort: high", `effort: ""`)
	if localConfig == string(template) {
		t.Fatal("local harness command fixture was not replaced")
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(configPath, []byte(localConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err = runTask(t.Context(), c, "target", configPath, strings.NewReader(input), &output, auto, terminal)
	mu.Lock()
	defer mu.Unlock()
	return stats, output.String(), err
}

func runSpecTaskScenario(t *testing.T, input string, terminal bool) (taskRunStats, string, error) {
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
		if r.Header.Get("Authorization") != "Bearer user-credential" {
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
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			stats.getCalls++
			if stats.states["target-spec-1"] == core.WorkOrderCompleted {
				w.WriteHeader(http.StatusNoContent)
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
	command := `command: ["` + strings.ReplaceAll(os.Args[0], `"`, `\"`) + `", "-test.run=TestTaskRunHarnessHelper", "--", "{prompt}", "{mcp_config}"]`
	localConfig := strings.Replace(string(template), `command: [agent-cli, --prompt, "{prompt}", --mcp-config, "{mcp_config}"]`, command, 1)
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(configPath, []byte(localConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{base: server.URL, token: "user-credential", workspace: "demo"}
	var output bytes.Buffer
	err = runTask(t.Context(), c, "target", configPath, strings.NewReader(input), &output, false, terminal)
	mu.Lock()
	defer mu.Unlock()
	return stats, output.String(), err
}

func TestRunTaskExecutesConfirmedSpecAndStopsAtOperatorGate(t *testing.T) {
	stats, output, err := runSpecTaskScenario(t, "yes\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.states["target-spec-1"] != core.WorkOrderCompleted || stats.getCalls != 2 || stats.claimCalls != 1 || stats.planSubmits != 1 || stats.releaseCalls != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(stats.progress) != 1 || stats.progress[0] != "conveyor run mode: confirmed-per-stage" {
		t.Fatalf("progress=%q", stats.progress)
	}
	for _, want := range []string{"Next: spec work order target-spec-1", "Execution: harness local-agent, model gpt-5.6-sol, effort high, timeout 30m", "Proceed with spec?", "pending spec approval gate", "operator approval is required"} {
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
	err := runTaskWithPresentation(t.Context(), c, "target", filepath.Join(t.TempDir(), "unused.yaml"), strings.NewReader("kk\napprove\n"), &output, true, true, true, false)
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
		_, _ = io.WriteString(writer, "kk\napp")
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(writer, "rove\n")
	}()
	c := &client{base: server.URL, token: "parent-user-credential", workspace: "demo"}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", input, &output, true, true, true, false); err != nil {
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
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("approve\n"), &output, false, true, true, true); err != nil {
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
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("k\n  fix the race  \n"), &output, false, true, true, false); err != nil {
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
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", strings.NewReader(""), &output, true, true, true, false); err != nil {
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
	if err := runTaskWithPresentation(ctx, c, "target", "unused.yaml", input, &output, true, true, true, false); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || mutations != 0 || !strings.Contains(output.String(), "finished in state merged") {
		t.Fatalf("reads=%d mutations=%d output=%q", reads, mutations, output.String())
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
	if err := runTaskWithPresentation(t.Context(), c, "target", "unused.yaml", strings.NewReader("k\napprove\n"), &output, false, true, true, false); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || !strings.Contains(output.String(), "finished in state closed") {
		t.Fatalf("reads=%d output=%q", reads, output.String())
	}
}

func TestTaskRunProposalClientUsesExistingAuthenticatedEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		{Kind: "design", DocumentID: "design-run", Version: 7},
		{Kind: "decision", DocumentID: "DEC-9", Version: 1},
		{Kind: "plan_revision", DocumentID: "target", Version: 3},
	} {
		if err := c.confirmTaskRunProposalContext(t.Context(), c.token, "target", proposal); err != nil {
			t.Fatalf("confirm %+v: %v", proposal, err)
		}
	}
	want := []string{
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
			err := runStageWithTaskProposalPresentation(ctx, c, c.token, "target", nil, presentation, func() error {
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
	stats, output, err := runSpecTaskScenario(t, "no\n", true)
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
	stats, output, err := runTaskScenario(t, "yes\nyes\n", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.states["target-implement-1"] != core.WorkOrderSubmitted || stats.states["target-review-1"] != core.WorkOrderCompleted || stats.getCalls != 3 || stats.claimCalls != 2 || stats.reviewSubmits != 1 || stats.verdictSubmits != 1 || stats.releaseCalls != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(stats.progress) != 2 || stats.progress[0] != "conveyor run mode: confirmed-per-stage" || stats.progress[1] != "conveyor run mode: confirmed-per-stage" {
		t.Fatalf("progress=%q", stats.progress)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "Next: implement work order target-implement-1", "Proceed with implement?", "Next: review work order target-review-1", "Proceed with review?", "task target has no claimable spec, implement, or review order"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
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

	stats, output, err := runTaskScenario(t, "yes\nyes\n", false, true)
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
	stats, output, err := runTaskScenario(t, "no\n", false, true)
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
	stats, output, err := runTaskScenario(t, "yes\nno\n", false, true)
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

func TestRunTaskAutoPreservesChainAndRecordsMode(t *testing.T) {
	stats, output, err := runTaskScenario(t, "", true, false)
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
		t.Fatalf("auto output prompted: %q", output)
	}
}

func TestRunTaskNonTerminalPresentsAndDoesNotClaim(t *testing.T) {
	stats, output, err := runTaskScenario(t, "", false, false)
	if err == nil || !strings.Contains(err.Error(), "conveyor run target --auto") {
		t.Fatalf("err=%v", err)
	}
	if stats.claimCalls != 0 || stats.releaseCalls != 0 || len(stats.progress) != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	for _, want := range []string{"Task target: Ship target (state running)", "No work order was claimed", "conveyor run target --auto"} {
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
	err := runTask(t.Context(), c, "target", filepath.Join(t.TempDir(), "missing.yaml"), strings.NewReader(""), &output, true, false)
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
	err = runTask(t.Context(), c, "target", configPath, strings.NewReader(""), &output, true, false)
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

func TestRunTaskNoSelectedHarnessPresentsPendingOrderWithoutClaiming(t *testing.T) {
	claimCalls, output, err := runTaskSelectionErrorScenario(t, core.StageReview, 3, func(value string) string { return value })
	if err == nil || !strings.Contains(err.Error(), "no harness") || !strings.Contains(err.Error(), localExecutionSetupCommand) {
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
