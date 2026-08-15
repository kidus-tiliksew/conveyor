package main

import (
	"bytes"
	"encoding/json"
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
	states                              map[string]core.WorkOrderState
	getCalls, claimCalls, reviewSubmits int
	verdictSubmits, releaseCalls        int
	progress                            []string
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
	for _, want := range []string{"Task target: Ship target (state running)", "Next: implement work order target-implement-1", "Proceed with implement?", "Next: review work order target-review-1", "Proceed with review?", "task target has no claimable implement or review order"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
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
	if err == nil || !strings.Contains(err.Error(), "load local execution config") || !strings.Contains(err.Error(), "missing.yaml") {
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

func runTaskSelectionErrorScenario(t *testing.T, reviewSeat int, mutateConfig func(string) string) (int, string, error) {
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
				Order: core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewSeat: reviewSeat},
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
	claimCalls, output, err := runTaskSelectionErrorScenario(t, 1, func(value string) string {
		value = strings.Replace(value, "      review:\n        execution: mcp\n        timeout: 1h", "      review:\n        execution: in_process\n        timeout: 1h", 1)
		value = strings.Replace(value, "    review:\n      seats:\n        - {model: gpt-5.6, harness: local-agent}\n        - {model: claude-opus-4.1, harness: local-agent}", "    review:\n      seats:\n        - {model: gpt-5.6}", 1)
		return value
	})
	if err == nil || !strings.Contains(err.Error(), "no MCP route for review") {
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

func TestRunTaskNoSelectedHarnessPresentsPendingOrderWithoutClaiming(t *testing.T) {
	claimCalls, output, err := runTaskSelectionErrorScenario(t, 3, func(value string) string { return value })
	if err == nil || !strings.Contains(err.Error(), "no harness") {
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
	if strings.Contains(os.Getenv("CONVEYOR_WORK_ORDER_ID"), "implement") {
		call("submit_for_review")
	} else {
		call("submit_review_verdict")
	}
}
