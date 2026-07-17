package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigExportImportRoundTripsContextualSettings(t *testing.T) {
	document := cliContextualWorkspaceDocument()
	var imported config.WorkspaceDocument
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "demo" {
			t.Fatalf("X-Workspace-ID = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspace/config":
			_ = json.NewEncoder(w).Encode(config.VersionedDocument{Document: document, Version: 7})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspace/config":
			if got := r.Header.Get("If-Match"); got != "7" {
				t.Fatalf("If-Match = %q", got)
			}
			var request struct {
				Document config.WorkspaceDocument `json:"document"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			imported = request.Document
			_ = json.NewEncoder(w).Encode(config.UpdateReceipt{VersionedDocument: config.VersionedDocument{Document: imported, Version: 8}, EventID: 9})
		default:
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv("CONVEYOR_ADDR", srv.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "secret-token")
	previousWorkspace := workspaceFlag
	workspaceFlag = "demo"
	defer func() { workspaceFlag = previousWorkspace }()

	var exported bytes.Buffer
	export := configCmd()
	export.SetArgs([]string{"export"})
	export.SetOut(&exported)
	if err := export.Execute(); err != nil {
		t.Fatal(err)
	}
	var decoded config.WorkspaceDocument
	decoder := yaml.NewDecoder(strings.NewReader(exported.String()))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	assertContextualCLIConfig(t, decoded, "export")

	var output bytes.Buffer
	importCommand := configCmd()
	importCommand.SetArgs([]string{"import", "-"})
	importCommand.SetIn(strings.NewReader(exported.String()))
	importCommand.SetOut(&output)
	if err := importCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "updated workspace config v7 -> v8 (event 9)") {
		t.Fatalf("import output = %q", output.String())
	}
	assertContextualCLIConfig(t, imported, "import")
}

func cliContextualWorkspaceDocument() config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeoutText: "24h",
		ExecutionSettings: &config.ContextualExecutionSettings{
			ControlPlane: config.ControlPlaneSettings{
				Triage: config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "20m"},
				Spec:   config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "30m"},
			},
			Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt-implement", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h", FallbackModel: "fallback", FallbackHarness: "codex"},
		},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", TimeoutText: "20m", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt", TimeoutText: "30m", Execution: config.ExecutionInProcess},
			"implement": {Model: "gpt-implement", ModelPolicy: config.ModelPolicyExplicit, Harness: "codex", TimeoutText: "2h", Execution: config.ExecutionMCP},
			"review":    {Model: "fallback", Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Harnesses: []config.Harness{
			{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
			{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{
			{Model: "gpt-review", Effort: "high"},
			{Model: "claude-review", Harness: "claude", Effort: "high"},
		}},
		Execution: config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 2},
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}
}

func assertContextualCLIConfig(t *testing.T, document config.WorkspaceDocument, step string) {
	t.Helper()
	if document.ExecutionSettings == nil || document.ExecutionSettings.Implementation.Harness != "codex" || document.ExecutionSettings.Review.FallbackHarness != "codex" {
		t.Fatalf("%s lost execution_settings: %+v", step, document.ExecutionSettings)
	}
	if len(document.Review.Seats) != 2 || document.Review.Seats[1].Harness != "claude" || document.Review.Seats[1].Effort != "high" {
		t.Fatalf("%s lost review seat effort: %+v", step, document.Review.Seats)
	}
	if document.Routing.Stages["implement"].Harness != "codex" || document.Routing.Stages["review"].Model != "fallback" {
		t.Fatalf("%s lost compatibility routing: %+v", step, document.Routing.Stages)
	}
}
