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

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestRunTaskExecutesImplementReviewChainThenExitsWithoutPolling(t *testing.T) {
	t.Setenv("CONVEYOR_FAKE_TASK_RUN_HARNESS", "1")
	var mu sync.Mutex
	states := map[string]core.WorkOrderState{
		"target-implement-1": core.WorkOrderQueued,
		"target-review-1":    core.WorkOrderQueued,
	}
	sessions := map[string]string{}
	getCalls, claimCalls, reviewSubmits, verdictSubmits, releaseCalls := 0, 0, 0, 0, 0
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
			if request.Params.Arguments["session_id"] != sessions[orderID] || states[orderID] == "" {
				http.Error(w, "wrong task session", http.StatusForbidden)
				return
			}
			switch request.Params.Name {
			case "get_work_order":
			case "submit_for_review":
				if orderID != "target-implement-1" {
					http.Error(w, "wrong implement order", http.StatusBadRequest)
					return
				}
				reviewSubmits++
				states[orderID] = core.WorkOrderSubmitted
			case "submit_review_verdict":
				if orderID != "target-review-1" {
					http.Error(w, "wrong review order", http.StatusBadRequest)
					return
				}
				verdictSubmits++
				states[orderID] = core.WorkOrderCompleted
			default:
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ok":true}`}}}})
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/target/run-order":
			getCalls++
			order := core.WorkOrder{ID: "target-implement-1", TaskID: "target", Stage: core.StageImplement, State: states["target-implement-1"]}
			if states["target-implement-1"] == core.WorkOrderSubmitted {
				order = core.WorkOrder{ID: "target-review-1", TaskID: "target", Stage: core.StageReview, State: states["target-review-1"], ReviewSeat: 1}
			}
			if states["target-review-1"] == core.WorkOrderCompleted {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
				Order:    order,
				Task:     core.Task{ID: "target", Repo: "conveyor", Branch: "conveyor/task-target", BaseBranch: "main"},
				Dispatch: "run", Auth: "user",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			claimCalls++
			var claim struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			orderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/target/run-orders/"), "/claim")
			sessions[orderID], states[orderID] = claim.SessionID, core.WorkOrderClaimed
			stage := core.StageImplement
			if orderID == "target-review-1" {
				stage = core.StageReview
			}
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: orderID, TaskID: "target", Stage: stage, State: states[orderID], AttemptID: "attempt-" + orderID, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reconcile"):
			orderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/target/run-orders/"), "/reconcile")
			_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: orderID, State: states[orderID]}, Authorized: states[orderID] == core.WorkOrderClaimed})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releaseCalls++
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
	if err = runTask(t.Context(), c, "target", configPath, &output); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if states["target-implement-1"] != core.WorkOrderSubmitted || states["target-review-1"] != core.WorkOrderCompleted || getCalls != 3 || claimCalls != 2 || reviewSubmits != 1 || verdictSubmits != 1 || releaseCalls != 0 {
		t.Fatalf("states=%+v gets=%d claims=%d review_submits=%d verdict_submits=%d releases=%d", states, getCalls, claimCalls, reviewSubmits, verdictSubmits, releaseCalls)
	}
	if !strings.Contains(output.String(), "task target has no claimable implement or review order") {
		t.Fatalf("output=%q", output.String())
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
