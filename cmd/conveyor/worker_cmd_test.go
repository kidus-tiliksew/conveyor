package main

import (
	"bytes"
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
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestExpandHarnessUsesWholeElementSubstitutionAndOptionalModelArgs(t *testing.T) {
	harness := config.Harness{Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}}
	want := []string{"codex", "exec", "do work", "--config", "/tmp/mcp.json", "--model", "gpt"}
	if got := expandHarness(harness, "gpt", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%q want=%q", got, want)
	}
	want = want[:5]
	if got := expandHarness(harness, "", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("without model argv=%q want=%q", got, want)
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
