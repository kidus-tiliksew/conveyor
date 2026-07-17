package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestExpandHarnessUsesWholeElementSubstitutionAndOptionalModelArgs(t *testing.T) {
	harness := config.Harness{Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}}
	want := []string{"codex", "exec", "do work", "--config", "/tmp/mcp.json", "--model", "gpt"}
	if got := expandHarness(harness, "gpt", "", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%q want=%q", got, want)
	}
	want = want[:5]
	if got := expandHarness(harness, "", "", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("without model argv=%q want=%q", got, want)
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
	if !strings.Contains(prompt, "call get_work_order with that exact session_id") || !strings.Contains(prompt, "standard review lifecycle") {
		t.Fatalf("review prompt lost its existing lifecycle instructions: %s", prompt)
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
	dispatcher.UseDurableQueue()
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
	dispatcher.UseDurableQueue()
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
	var mu sync.Mutex
	sessions := map[string]string{}
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
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&claim)
			sessions[orderID] = claim.SessionID
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
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		orderID := "fake-" + string(stage)
		item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: orderID, Stage: stage}, Harness: harness, Model: "fake-model"}
		if err := runHarnessChild(t.Context(), c, "worker-credential", item); err != nil {
			t.Fatalf("%s child: %v", stage, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if states["fake-implement"] != core.WorkOrderSubmitted || states["fake-review"] != core.WorkOrderCompleted || called["get_work_order"] != 2 || called["submit_for_review"] != 1 || called["submit_review_verdict"] != 1 || releases != 0 {
		t.Fatalf("states=%+v called=%+v releases=%d", states, called, releases)
	}
}

func TestWorkerHarnessHelper(t *testing.T) {
	if os.Getenv("CONVEYOR_FAKE_HARNESS") != "1" {
		return
	}
	if len(os.Args) < 3 {
		t.Fatal("missing prompt and MCP config arguments")
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
	if strings.Contains(orderID, "implement") {
		call("submit_for_review", identity)
	} else {
		identity["verdict"] = "approve"
		identity["reason_code"] = "fake"
		identity["summary"] = fmt.Sprintf("reviewed %s", orderID)
		call("submit_review_verdict", identity)
	}
}
