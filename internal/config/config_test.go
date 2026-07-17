package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceDocumentEmitsEmptyCollectionsAsArrays(t *testing.T) {
	cfg := validConfig()
	cfg.Harnesses = nil
	cfg.Repos = nil
	document := cfg.WorkspaceDocument()
	if document.Harnesses == nil || document.Repos == nil {
		t.Fatalf("workspace document contains nil collections: %+v", document)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"harnesses":[]`) || !strings.Contains(string(data), `"repos":[]`) {
		t.Fatalf("workspace document JSON contains nullable collections: %s", data)
	}
}

func TestLoadPhase47Config(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `workspace: demo
pack_dir: pack
max_bounces: 2
work_order_queue_timeout: 36h
routing:
  stages:
    triage: {model: gpt-5.4, timeout: 20m, execution: in_process}
    spec: {model: gpt-5.4, timeout: 30m, execution: in_process}
    implement: {model: operator-owned, timeout: 4h, execution: mcp}
    review: {model: operator-owned, timeout: 1h, execution: mcp}
repos:
  - {name: conveyor, url: https://example.test/conveyor, base: main}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Stages["implement"].Execution != ExecutionMCP || cfg.Repos[0].Name != "conveyor" || cfg.WorkOrderQueueTimeout != 36*time.Hour {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestExampleUsesContextualSettingsWithoutLiteralSubscriptionModel(t *testing.T) {
	cfg, err := Load("../../conveyor.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	implement := cfg.Routing.Stages["implement"]
	if cfg.ExecutionSettings == nil || implement.ModelPolicy != ModelPolicyHarnessDefault || cfg.EffectiveModel("implement") != "" {
		t.Fatalf("implementation settings=%+v effective_model=%q", cfg.ExecutionSettings, cfg.EffectiveModel("implement"))
	}
	if len(cfg.Review.Seats) != 2 || cfg.Routing.Stages["review"].Harness != "" {
		t.Fatalf("review settings=%+v route=%+v", cfg.Review, cfg.Routing.Stages["review"])
	}
}

func TestWorkOrderQueueTimeoutDefaultsAndRejectsInvalidDuration(t *testing.T) {
	deployment := validConfig()
	data, err := yaml.Marshal(deployment.WorkspaceDocument())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseWorkspaceDocument(data, deployment, "test")
	if err != nil || parsed.WorkOrderQueueTimeout != DefaultWorkOrderQueueTimeout || parsed.WorkOrderQueueTimeoutText != DefaultWorkOrderQueueTimeoutText {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	document := deployment.WorkspaceDocument()
	document.WorkOrderQueueTimeoutText = "never"
	invalid, marshalErr := yaml.Marshal(document)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if _, err = ParseWorkspaceDocument(invalid, deployment, "test"); err == nil || !strings.Contains(err.Error(), "work_order_queue_timeout") {
		t.Fatalf("invalid timeout error=%v", err)
	}
}

func TestLoadRejectsRemovedBudgetConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `workspace: demo
pack_dir: pack
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, budget_usd: 10, timeout: 4h, execution: mcp}
    review: {model: operator, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field budget_usd not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseStoredWorkspaceDocumentRemovesLegacyBudget(t *testing.T) {
	data := `workspace: demo
max_bounces: 2
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, budget_usd: 10, timeout: 4h, execution: mcp}
    review: {model: operator, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	cfg, legacy, err := ParseStoredWorkspaceDocument([]byte(data), validConfig(), "stored")
	if err != nil || !legacy {
		t.Fatalf("legacy=%t err=%v", legacy, err)
	}
	canonical, err := MarshalWorkspaceDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "budget") {
		t.Fatalf("canonical config retained budget: %s", canonical)
	}
}

func TestLoadRejectsRetiredConfigSurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, []byte("workspace: demo\ncredentials: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field credentials not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestReviewMayUseInProcessFallback(t *testing.T) {
	deployment := validConfig()
	document := deployment.WorkspaceDocument()
	route := document.Routing.Stages["review"]
	route.Execution = ExecutionInProcess
	document.Routing.Stages["review"] = route
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseWorkspaceDocument(raw, deployment, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessRegistryValidatesFieldLocalTemplatesAndRoutes(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	document.Harnesses = []Harness{{Name: "codex", Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}}
	implement := document.Routing.Stages["implement"]
	implement.Harness = ""
	document.Routing.Stages["implement"] = implement
	review := document.Routing.Stages["review"]
	review.Harness = "codex"
	document.Routing.Stages["review"] = review
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Routing.Stages["implement"].Harness != "codex" || parsed.Harnesses[0].ProbeTimeout != 5*time.Second || parsed.Harnesses[0].MCPTransport != MCPTransportJSONFile {
		t.Fatalf("parsed=%+v", parsed)
	}

	tests := []struct {
		name   string
		mutate func(*WorkspaceDocument)
		want   string
	}{
		{"wrong field", func(d *WorkspaceDocument) { d.Harnesses[0].ProbeCommand = []string{"codex", "{model}"} }, "probe_command"},
		{"missing prompt", func(d *WorkspaceDocument) { d.Harnesses[0].Command = []string{"codex", "{mcp_config}"} }, "exactly one"},
		{"embedded placeholder", func(d *WorkspaceDocument) {
			d.Harnesses[0].Command = []string{"codex", "prefix-{prompt}", "{mcp_config}"}
		}, "whole argv"},
		{"non-positive timeout", func(d *WorkspaceDocument) { d.Harnesses[0].ProbeTimeoutText = "0s" }, "positive duration"},
		{"unknown MCP transport", func(d *WorkspaceDocument) { d.Harnesses[0].MCPTransport = "provider_magic" }, "mcp_transport"},
		{"dangling route", func(d *WorkspaceDocument) {
			route := d.Routing.Stages["review"]
			route.Harness = "missing"
			d.Routing.Stages["review"] = route
			d.ExecutionSettings.Review.FallbackHarness = "missing"
		}, "unknown harness"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := document
			candidate.Harnesses = append([]Harness(nil), document.Harnesses...)
			candidate.Routing.Stages = map[string]StageRoute{}
			for key, value := range document.Routing.Stages {
				candidate.Routing.Stages[key] = value
			}
			test.mutate(&candidate)
			data, _ := yaml.Marshal(candidate)
			if _, err := ParseWorkspaceDocument(data, base, "test"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestMultipleHarnessesRequireExplicitWorkerRoute(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	document.Harnesses = []Harness{
		{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
		{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
	}
	raw, _ := yaml.Marshal(document)
	if _, err := ParseWorkspaceDocument(raw, base, "test"); err == nil || !strings.Contains(err.Error(), "harness is required") {
		t.Fatalf("error=%v", err)
	}
}

func TestContextualSettingsOverrideLegacyRoutesAndAllowExplicitReviewSeats(t *testing.T) {
	base := validConfig()
	base.Harnesses = []Harness{
		{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
		{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
	}
	base.Routing.Stages["implement"] = StageRoute{Model: "subscription", Harness: "codex", TimeoutText: "1h", Execution: ExecutionMCP}
	base.Routing.Stages["review"] = StageRoute{Model: "stale", Harness: "stale-harness", TimeoutText: "1h", Execution: ExecutionMCP}
	base.Review.Seats = []ReviewSeat{{Model: "gpt-review", Harness: "codex"}, {Model: "claude-review", Harness: "claude"}}
	document := base.WorkspaceDocument()
	document.ExecutionSettings.Implementation.Model = ""
	document.ExecutionSettings.Implementation.ModelPolicy = ModelPolicyHarnessDefault
	document.ExecutionSettings.Review.FallbackModel = ""
	document.ExecutionSettings.Review.FallbackHarness = ""
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "contextual settings test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Routing.Stages["review"].Harness != "" || parsed.EffectiveModel("implement") != "" {
		t.Fatalf("normalized routes=%+v", parsed.Routing.Stages)
	}
	canonical, err := MarshalWorkspaceDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), "execution_settings:") || !strings.Contains(string(canonical), "routing:") {
		t.Fatalf("canonical document lost contextual or compatibility shape: %s", canonical)
	}
}

func TestHarnessDefaultModelOnlyForwardsDeclaredSentinel(t *testing.T) {
	base := validConfig()
	base.Harnesses = []Harness{{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, DefaultModelSentinels: []string{"subscription"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}}
	base.Routing.Stages["implement"] = StageRoute{Model: "subscription", ModelPolicy: ModelPolicyHarnessDefault, Harness: "codex", TimeoutText: "1h", Execution: ExecutionMCP}
	document := base.WorkspaceDocument()
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "declared sentinel test")
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.EffectiveModel("implement"); got != "subscription" {
		t.Fatalf("effective model=%q", got)
	}

	document.ExecutionSettings.Implementation.Model = "unsupported-sentinel"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "undeclared sentinel test"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared sentinel error=%v", err)
	}
}

func TestExplicitSymbolicModelRequiresHarnessDefaultPolicy(t *testing.T) {
	route := StageRoute{Model: "subscription", ModelPolicy: ModelPolicyExplicit, Harness: "codex"}
	harnesses := []Harness{{Name: "codex", DefaultModelSentinels: []string{"subscription"}}}
	if _, err := normalizeHarnessModel(route, harnesses); err == nil || !strings.Contains(err.Error(), `symbolic model "subscription" requires harness_default model policy`) {
		t.Fatalf("explicit symbolic model error=%v", err)
	}
}

func TestReviewPanelValidatesOrderedPinnedSeatsAndHarnessOverrides(t *testing.T) {
	base := validConfig()
	base.Harnesses = []Harness{
		{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
		{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
	}
	base.Routing.Stages["implement"] = StageRoute{Model: "implementer", Harness: "codex", TimeoutText: "1h", Execution: ExecutionMCP}
	base.Routing.Stages["review"] = StageRoute{Model: "fallback", Harness: "codex", TimeoutText: "1h", Execution: ExecutionMCP}
	document := base.WorkspaceDocument()
	document.Review.Seats = []ReviewSeat{{Model: "gpt-5.6-sol", Harness: "codex", Effort: "high"}, {Model: "claude-opus-4-8", Harness: "claude", Effort: "high"}}
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "review panel test")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Review.Seats) != 2 || parsed.Review.Seats[0].Effort != "high" || parsed.Review.Seats[1].Harness != "claude" || parsed.Review.Seats[1].Effort != "high" {
		t.Fatalf("review seats=%+v", parsed.Review.Seats)
	}
	encoded, _ := json.Marshal(parsed.WorkspaceDocument().Review.Seats)
	if !strings.Contains(string(encoded), `"effort":"high"`) {
		t.Fatalf("effort did not round trip: %s", encoded)
	}

	document.Review.Seats[1].Harness = "missing"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "review panel test"); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("unknown harness error=%v", err)
	}
	document.Review.Seats = []ReviewSeat{{Model: ""}}
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "review panel test"); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("missing model error=%v", err)
	}
	document.Review.Seats = []ReviewSeat{{Model: "gpt", Harness: "codex", Effort: "ultrathink"}}
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "review panel test"); err == nil || !strings.Contains(err.Error(), "must be low, medium, or high") {
		t.Fatalf("invalid effort error=%v", err)
	}
	document.Review.Seats[0].Effort = "medium"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "review panel test"); err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Fatalf("unsupported effort error=%v", err)
	}
	legacy := []ReviewSeat{{Model: "gpt", Harness: "codex"}}
	encoded, _ = json.Marshal(legacy)
	if strings.Contains(string(encoded), "effort") {
		t.Fatalf("legacy seat gained effort: %s", encoded)
	}
}

func TestLoadRejectsExplicitlyEmptyReviewPanel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `workspace: demo
pack_dir: pack
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, timeout: 4h, execution: mcp}
    review: {model: reviewer, timeout: 1h, execution: mcp}
review:
  seats: []
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "at least one seat") {
		t.Fatalf("empty review panel error=%v", err)
	}
}

func validConfig() *Config {
	return &Config{Workspace: "demo", PackDir: ".", MaxBounces: 2, Database: Database{Backend: "memory"}, Routing: Routing{Stages: map[string]StageRoute{"triage": {Model: "gpt", TimeoutText: "20m", Execution: ExecutionInProcess}, "spec": {Model: "gpt", TimeoutText: "30m", Execution: ExecutionInProcess}, "implement": {Model: "operator", TimeoutText: "4h", Execution: ExecutionMCP}, "review": {Model: "operator", TimeoutText: "1h", Execution: ExecutionMCP}}}, Repos: []Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
}
