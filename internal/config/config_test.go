package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
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

func TestWorkspaceDocumentEmitsSetupReviewSeatsAsArrays(t *testing.T) {
	cfg := validConfig()
	normalized, err := normalize(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	document := normalized.WorkspaceDocument()
	if len(document.Setups) != 1 || document.Setups[0].Review.Seats == nil {
		t.Fatalf("workspace document contains nullable setup review seats: %+v", document.Setups)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"setups":[`) || strings.Contains(string(data), `"seats":null`) {
		t.Fatalf("workspace document JSON contains nullable setup review seats: %s", data)
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
	templates := HarnessTemplates()
	if len(templates) != 3 || !reflect.DeepEqual(templates[1].Harness.Command, []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--allowedTools", "mcp__conveyor__*"}) {
		t.Fatalf("Claude catalog template does not pre-authorize the scoped Conveyor MCP lifecycle: %+v", templates)
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

func TestEnvironmentHarnessRequiresNonSecretAttachmentAndTransportAwarePlaceholders(t *testing.T) {
	valid := Harness{
		Name: "grok", MCPTransport: MCPTransportEnvironment, MCPAttachment: "conveyor",
		Command:   []string{"grok", "--single", "{prompt}", "--permission-mode", "bypassPermissions", "--no-plan"},
		ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--reasoning-effort", "high"}},
		ProbeCommand: []string{"grok", "--version"}, ProbeTimeoutText: "30s",
	}
	if err := ValidateHarness(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Harness)
		want   string
	}{
		{"environment config placeholder", func(h *Harness) { h.Command = append(h.Command, "{mcp_config}") }, "must not contain"},
		{"missing attachment", func(h *Harness) { h.MCPAttachment = "" }, "mcp_attachment"},
		{"URL attachment", func(h *Harness) { h.MCPAttachment = "https://example.test/mcp" }, "mcp_attachment"},
		{"runtime token in command", func(h *Harness) { h.Command = append(h.Command, "CONVEYOR_API_TOKEN") }, "runtime Conveyor"},
		{"token-bearing endpoint", func(h *Harness) { h.Command = append(h.Command, "https://example.test/mcp?token=persisted") }, "runtime Conveyor"},
		{"runtime address in effort", func(h *Harness) { h.EffortArgs = map[string][]string{"high": {"${CONVEYOR_ADDR}"}} }, "runtime Conveyor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Command = append([]string(nil), valid.Command...)
			candidate.EffortArgs = map[string][]string{"high": append([]string(nil), valid.EffortArgs["high"]...)}
			test.mutate(&candidate)
			if err := ValidateHarness(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	for _, transport := range []string{MCPTransportJSONFile, MCPTransportTOMLOverride} {
		legacy := valid
		legacy.MCPTransport = transport
		legacy.MCPAttachment = ""
		legacy.Command = []string{"agent", "{prompt}", "{mcp_config}"}
		if err := ValidateHarness(legacy); err != nil {
			t.Fatalf("%s compatibility: %v", transport, err)
		}
		legacy.Command = []string{"agent", "{prompt}"}
		if err := ValidateHarness(legacy); err == nil || !strings.Contains(err.Error(), "exactly one {mcp_config}") {
			t.Fatalf("%s missing placeholder error=%v", transport, err)
		}
	}
}

func TestHarnessTemplatesMatchValidationContract(t *testing.T) {
	templates := HarnessTemplates()
	if len(templates) != 3 {
		t.Fatalf("template count = %d, want 3", len(templates))
	}
	wantIDs := []string{"codex", "claude", "grok"}
	for index, template := range templates {
		if template.ID != wantIDs[index] {
			t.Fatalf("template %d id = %q, want %q", index, template.ID, wantIDs[index])
		}
		if template.Label == "" || template.Description == "" {
			t.Fatalf("template %q is missing picker copy", template.ID)
		}
		if template.Harness.Name != template.ID {
			t.Fatalf("template %q harness name = %q", template.ID, template.Harness.Name)
		}
		if err := ValidateHarness(template.Harness); err != nil {
			t.Fatalf("template %q failed validation: %v", template.ID, err)
		}
	}
	if templates[0].Harness.MCPTransport != MCPTransportTOMLOverride || templates[1].Harness.MCPTransport != MCPTransportJSONFile {
		t.Fatalf("codex/claude transports = %q/%q", templates[0].Harness.MCPTransport, templates[1].Harness.MCPTransport)
	}
	grok := templates[2].Harness
	if grok.MCPTransport != MCPTransportEnvironment || grok.MCPAttachment != "conveyor" {
		t.Fatalf("grok transport/attachment = %q/%q", grok.MCPTransport, grok.MCPAttachment)
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

func TestImplementationEffortValidatesSelectedHarnessAndPreservesUnset(t *testing.T) {
	base := validConfig()
	base.Harnesses = []Harness{{
		Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"},
		ModelArgs:    []string{"--model", "{model}"},
		EffortArgs:   map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}},
		ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s",
	}}
	base.Routing.Stages["implement"] = StageRoute{Model: "gpt", ModelPolicy: ModelPolicyExplicit, Harness: "codex", TimeoutText: "1h", Execution: ExecutionMCP}
	document := base.WorkspaceDocument()
	document.ExecutionSettings.Implementation.Effort = "high"
	document.Routing.Stages["implement"] = StageRoute{Model: "stale", Harness: "stale", Effort: "low", TimeoutText: "1m", Execution: ExecutionMCP}
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "implementation effort test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExecutionSettings.Implementation.Effort != "high" || parsed.Routing.Stages["implement"].Effort != "high" {
		t.Fatalf("implementation effort did not round trip: settings=%+v route=%+v", parsed.ExecutionSettings.Implementation, parsed.Routing.Stages["implement"])
	}
	encoded, _ := json.Marshal(parsed.WorkspaceDocument())
	if !strings.Contains(string(encoded), `"effort":"high"`) {
		t.Fatalf("implementation effort omitted: %s", encoded)
	}

	document.ExecutionSettings.Implementation.Effort = "medium"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "implementation effort test"); err == nil || !strings.Contains(err.Error(), `execution_settings.implementation.effort "medium" is not supported by harness "codex"`) {
		t.Fatalf("unsupported effort error=%v", err)
	}
	document.ExecutionSettings.Implementation.Effort = "ultrathink"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "implementation effort test"); err == nil || !strings.Contains(err.Error(), "execution_settings.implementation.effort must be low, medium, or high") {
		t.Fatalf("invalid effort error=%v", err)
	}

	document = base.WorkspaceDocument()
	raw, _ = yaml.Marshal(document)
	parsed, err = ParseWorkspaceDocument(raw, base, "implementation effort unset test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(parsed.WorkspaceDocument().ExecutionSettings.Implementation)
	if strings.Contains(string(encoded), "effort") {
		t.Fatalf("unset implementation effort changed legacy serialization: %s", encoded)
	}
}

func TestControlPlaneEffortRoundTripsIntoStageRoutes(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	document.ExecutionSettings.ControlPlane.Triage.Effort = "minimal"
	document.ExecutionSettings.ControlPlane.Spec.Effort = "high"
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseWorkspaceDocument(raw, base, "control-plane effort test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExecutionSettings.ControlPlane.Triage.Effort != "minimal" || parsed.Routing.Stages["triage"].Effort != "minimal" {
		t.Fatalf("triage effort did not reach route: settings=%+v route=%+v", parsed.ExecutionSettings.ControlPlane.Triage, parsed.Routing.Stages["triage"])
	}
	if parsed.ExecutionSettings.ControlPlane.Spec.Effort != "high" || parsed.Routing.Stages["spec"].Effort != "high" {
		t.Fatalf("spec effort did not reach route: settings=%+v route=%+v", parsed.ExecutionSettings.ControlPlane.Spec, parsed.Routing.Stages["spec"])
	}
	encoded, err := json.Marshal(parsed.WorkspaceDocument().ExecutionSettings.ControlPlane)
	if err != nil || !strings.Contains(string(encoded), `"effort":"minimal"`) || !strings.Contains(string(encoded), `"effort":"high"`) {
		t.Fatalf("control-plane efforts did not round trip: %s err=%v", encoded, err)
	}

	document.ExecutionSettings.ControlPlane.Triage.Effort = "maximum"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "control-plane effort test"); err == nil || !strings.Contains(err.Error(), `execution_settings.control_plane.triage.effort "maximum" must be minimal, low, medium, or high`) {
		t.Fatalf("invalid triage effort error=%v", err)
	}

	document = base.WorkspaceDocument()
	raw, _ = yaml.Marshal(document)
	parsed, err = ParseWorkspaceDocument(raw, base, "control-plane effort unset test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(parsed.WorkspaceDocument().ExecutionSettings.ControlPlane)
	if strings.Contains(string(encoded), "effort") {
		t.Fatalf("unset control-plane effort changed legacy serialization: %s", encoded)
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

func TestExecutionSetupsNormalizeLegacyAndProjectDefault(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	document.Setups = nil
	document.DefaultSetup = ""
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseWorkspaceDocument(raw, base, "legacy-v1.27")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DefaultSetup != "default" || len(parsed.Setups) != 1 || parsed.Setups[0].Name != "default" {
		t.Fatalf("legacy setup normalization = %+v default=%q", parsed.Setups, parsed.DefaultSetup)
	}
	if parsed.Setups[0].RefreshReview != RefreshReviewDelta {
		t.Fatalf("legacy refresh_review = %q", parsed.Setups[0].RefreshReview)
	}
	projected := parsed.WorkspaceDocument()
	if projected.ExecutionSettings == nil || *projected.ExecutionSettings != parsed.Setups[0].ExecutionSettings || !reflect.DeepEqual(projected.Review, parsed.Setups[0].Review) {
		t.Fatalf("legacy projection diverged: document=%+v setup=%+v", projected, parsed.Setups[0])
	}
}

func TestExecutionSetupRefreshReviewValidation(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	document.Setups = []ExecutionSetup{{Name: "default", ExecutionSettings: *document.ExecutionSettings, Review: document.Review, RefreshReview: RefreshReviewFull}}
	document.DefaultSetup = "default"
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "refresh-review")
	if err != nil || parsed.Setups[0].RefreshReview != RefreshReviewFull {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	document.Setups[0].RefreshReview = "sometimes"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "refresh-review"); err == nil || !strings.Contains(err.Error(), "refresh_review") {
		t.Fatalf("invalid refresh_review error=%v", err)
	}
}

func TestExecutionSetupsValidateEveryHarnessReferenceAndDefault(t *testing.T) {
	base := validConfig()
	document := base.WorkspaceDocument()
	harness := func(name string) Harness {
		return Harness{Name: name, MCPTransport: MCPTransportJSONFile, Command: []string{name, "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{name, "--version"}, ProbeTimeoutText: "5s"}
	}
	settings := func(harnessName, model string) ContextualExecutionSettings {
		return ContextualExecutionSettings{
			ControlPlane:   ControlPlaneSettings{Triage: ModelTimeoutSettings{Model: "gpt-control", TimeoutText: "20m"}, Spec: ModelTimeoutSettings{Model: "gpt-control", TimeoutText: "30m"}},
			Implementation: ImplementationSettings{Harness: harnessName, Model: model, ModelPolicy: ModelPolicyExplicit, TimeoutText: "2h"},
			Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "1h"},
		}
	}
	document.Harnesses = []Harness{harness("codex"), harness("claude")}
	document.Setups = []ExecutionSetup{
		{Name: "backend", ExecutionSettings: settings("codex", "gpt-backend"), Review: ReviewPanel{Seats: []ReviewSeat{{Model: "gpt-review", Harness: "codex"}}}},
		{Name: "frontend", ExecutionSettings: settings("claude", "claude-ui"), Review: ReviewPanel{Seats: []ReviewSeat{{Model: "claude-review", Harness: "claude"}}}},
	}
	document.DefaultSetup = "backend"
	raw, _ := yaml.Marshal(document)
	parsed, err := ParseWorkspaceDocument(raw, base, "setups")
	if err != nil || parsed.ExecutionSettings.Implementation.Model != "gpt-backend" || parsed.Review.Seats[0].Model != "gpt-review" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	document.Harnesses = document.Harnesses[:1]
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "setups"); err == nil || !strings.Contains(err.Error(), `setup "frontend"`) || !strings.Contains(err.Error(), `unknown harness "claude"`) {
		t.Fatalf("deleted referenced harness error=%v", err)
	}
	document.Harnesses = []Harness{harness("codex"), harness("claude")}
	document.DefaultSetup = "missing"
	raw, _ = yaml.Marshal(document)
	if _, err = ParseWorkspaceDocument(raw, base, "setups"); err == nil || !strings.Contains(err.Error(), "default_setup") {
		t.Fatalf("invalid default error=%v", err)
	}
	document.Setups = []ExecutionSetup{}
	document.DefaultSetup = ""
	raw, _ = yaml.Marshal(document)
	raw = append(raw, []byte("setups: []\n")...)
	if _, err = ParseWorkspaceDocument(raw, base, "setups"); err == nil || !strings.Contains(err.Error(), "at least one setup") {
		t.Fatalf("empty setups error=%v", err)
	}
}
