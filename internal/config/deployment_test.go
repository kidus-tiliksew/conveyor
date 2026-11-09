package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// legacyInitShapedDeployment renders the deployment file that conveyor init
// wrote before DEC-56: control-plane models plus a full executor setup.
func legacyInitShapedDeployment(t *testing.T) []byte {
	t.Helper()
	harness := legacyHarnessYAML()["name"].(string)
	settings := ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage:   ModelTimeoutSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
			Planning: PlanningSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
		},
		Spec:           ImplementationSettings{Harness: harness, Model: "gpt-5.6-sol", ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: "30m"},
		Implementation: ImplementationSettings{Harness: harness, Model: "gpt-5.6-sol", ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: "4h"},
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "1h"},
	}
	review := ReviewPanel{Seats: []ReviewSeat{{Model: "gpt-5.6-terra", Harness: harness, Effort: "high"}}}
	return withLegacyExecutorKeys(t, Config{
		Workspace: "default", MaxBounces: 7,
		WorkOrderQueueTimeoutText: DefaultWorkOrderQueueTimeoutText,
		Database:                  DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		ExecutionSettings:         &settings,
		Review:                    review,
		Execution:                 ExecutionPolicy{DefaultPlanState: "needed", MergeApproval: false, ImplementConcurrency: 2, ReviewConcurrency: 1, FirstActivityTimeoutText: DefaultFirstActivityTimeoutText},
		Repos:                     []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", GitHub: "acme/conveyor", Base: "trunk", InstallConveyor: InstallSwitch(true)}},
		Monitor:                   MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	}, []map[string]any{legacySetupYAML(t, "default", settings, review, RefreshReviewDelta)}, "default")
}

// legacyHarnessYAML is one harness registry entry as earlier files wrote it.
// The registry is retired (DEC-104(2)); fixtures keep it only as authored YAML.
func legacyHarnessYAML() map[string]any {
	return map[string]any{
		"name": "codex", "mcp_transport": "toml_override",
		"command":       []any{"codex", "exec", "{prompt}", "{mcp_config}"},
		"probe_command": []any{"codex", "--version"}, "probe_timeout": "10s",
	}
}

// legacySetupYAML renders one execution setup entry as earlier files wrote it.
func legacySetupYAML(t *testing.T, name string, settings ContextualExecutionSettings, review ReviewPanel, refresh string) map[string]any {
	t.Helper()
	setup := map[string]any{"name": name, "execution_settings": yamlValue(t, settings), "review": yamlValue(t, review)}
	if refresh != "" {
		setup["refresh_review"] = refresh
	}
	return setup
}

// withLegacyExecutorKeys adds the retired harnesses, setups, and
// default_setup keys to a marshaled document.
func withLegacyExecutorKeys(t *testing.T, document Config, setups []map[string]any, defaultSetup string) []byte {
	t.Helper()
	root, ok := yamlValue(t, document).(map[string]any)
	if !ok {
		t.Fatal("document did not marshal to a mapping")
	}
	root["harnesses"] = []any{legacyHarnessYAML()}
	entries := make([]any, len(setups))
	for i, setup := range setups {
		entries[i] = setup
	}
	root["setups"] = entries
	root["default_setup"] = defaultSetup
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func yamlValue(t *testing.T, value any) any {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func writeDeploymentFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type warningLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *warningLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *warningLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func assertNoExecutorDetail(t *testing.T, cfg *Config) {
	t.Helper()
	for _, stage := range []string{"spec", "implement", "review", "verify"} {
		route := cfg.Routing.Stages[stage]
		if route.Model != "" || route.Harness != "" || route.Effort != "" || route.ModelPolicy != "" {
			t.Fatalf("deployment kept %s executor routing: %+v", stage, route)
		}
		if route.Execution != ExecutionMCP {
			t.Fatalf("deployment %s route execution = %q, want mcp", stage, route.Execution)
		}
	}
	for i, seat := range cfg.Review.Seats {
		if seat != (ReviewSeat{}) {
			t.Fatalf("deployment kept review seat %d contents: %+v", i, seat)
		}
	}
	settings := cfg.ExecutionSettings
	if settings == nil {
		t.Fatal("deployment lost its control-plane settings")
	}
	for name, stage := range map[string]ImplementationSettings{"spec": settings.Spec, "implementation": settings.Implementation, "verify": settings.Verify} {
		if stage.Harness != "" || stage.Model != "" || stage.Effort != "" || stage.ModelPolicy != "" {
			t.Fatalf("deployment kept %s execution settings: %+v", name, stage)
		}
	}
	if settings.Review.FallbackHarness != "" || settings.Review.FallbackModel != "" {
		t.Fatalf("deployment kept review fallbacks: %+v", settings.Review)
	}
}

// DEC-56(2), DEC-56(3); component-runtime "Configuration scopes": a file in
// the pre-DEC-56 init shape loads with its executor keys ignored and named,
// its policy and control-plane settings kept, and unknown keys still refused.
func TestDeploymentLoadIgnoresLegacyExecutorKeys(t *testing.T) {
	t.Run("earlier init shape", func(t *testing.T) {
		var warnings warningLog
		cfg, err := LoadDeployment(writeDeploymentFixture(t, legacyInitShapedDeployment(t)), warnings.logf)
		if err != nil {
			t.Fatal(err)
		}
		assertNoExecutorDetail(t, cfg)
		if cfg.Workspace != "default" || cfg.MaxBounces != 7 || cfg.Database.URL != "postgres://conveyor@db.invalid/conveyor" || cfg.Database.Backend != "postgres" {
			t.Fatalf("deployment identity changed: workspace=%q bounces=%d database=%+v", cfg.Workspace, cfg.MaxBounces, cfg.Database)
		}
		if cfg.Execution.DefaultPlanState != "needed" || cfg.Execution.MergeApproval || cfg.Execution.ImplementConcurrency != 2 || cfg.Execution.ReviewConcurrency != 1 {
			t.Fatalf("legacy policy values changed: %+v", cfg.Execution)
		}
		if len(cfg.Review.Seats) != 1 {
			t.Fatalf("review seat count = %d, want 1", len(cfg.Review.Seats))
		}
		for stage, want := range map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"} {
			if got := cfg.Routing.Stages[stage].TimeoutText; got != want {
				t.Fatalf("%s timeout = %q, want %q", stage, got, want)
			}
		}
		triage := cfg.Routing.Stages["triage"]
		if triage.Model != "gpt-5.6-luna" || triage.Effort != "high" || triage.TimeoutText != "20m" || triage.Execution != ExecutionInProcess {
			t.Fatalf("triage control-plane route = %+v", triage)
		}
		planning := cfg.ExecutionSettings.ControlPlane.Planning
		if planning.Model != "gpt-5.6-luna" || planning.Effort != "high" || planning.TimeoutText != "20m" {
			t.Fatalf("planning control-plane settings = %+v", planning)
		}
		if len(cfg.PlanningModels) != 1 || cfg.PlanningModels[0] != "gpt-5.6-luna" {
			t.Fatalf("planning models = %v", cfg.PlanningModels)
		}
		if len(cfg.Repos) != 1 || cfg.Repos[0].Base != "trunk" || cfg.Repos[0].GitHub != "acme/conveyor" {
			t.Fatalf("repositories = %+v", cfg.Repos)
		}
		lines := warnings.all()
		if len(lines) != 1 {
			t.Fatalf("warnings = %q, want one line", lines)
		}
		for _, name := range []string{"default_setup", "execution_settings.implementation.harness", "execution_settings.implementation.model", "execution_settings.implementation.model_policy", "execution_settings.implementation.effort", "execution_settings.spec.model", "execution_settings.review.execution", "harnesses", "review.seats[].effort", "review.seats[].harness", "review.seats[].model", "setups"} {
			if !strings.Contains(lines[0], name) {
				t.Fatalf("warning %q does not name %s", lines[0], name)
			}
		}
		for _, value := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "codex", "explicit"} {
			if strings.Contains(lines[0], value) {
				t.Fatalf("warning %q echoes value %q", lines[0], value)
			}
		}
	})
	t.Run("invalid retired values never validate", func(t *testing.T) {
		data := []byte(`workspace: default
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: gpt-luna, timeout: 20m}
  spec: {harness: "not a harness!", model: "", model_policy: bogus, effort: extreme, timeout: 45m}
  implementation: {harness: missing, model_policy: harness_default, effort: maximal, timeout: 3h}
  review: {execution: sideways, fallback_harness: missing, timeout: 50m}
harnesses:
  - {name: "bad name!", mcp_transport: carrier-pigeon, command: []}
setups:
  - {name: ""}
default_setup: nope
review:
  seats:
    - {model: "", harness: missing, effort: extreme}
    - {}
routing:
  stages:
    triage: {harness: codex, execution: mcp}
    implement: {model: x, model_policy: nonsense, harness: missing, harnesses: [a], model_tier: big, execution: in_process}
repos: []
`)
		var warnings warningLog
		cfg, err := ParseDeployment(data, "inline.yaml", warnings.logf)
		if err != nil {
			t.Fatal(err)
		}
		assertNoExecutorDetail(t, cfg)
		if len(cfg.Review.Seats) != 2 {
			t.Fatalf("seat count = %d, want 2", len(cfg.Review.Seats))
		}
		if cfg.Routing.Stages["spec"].TimeoutText != "45m" || cfg.Routing.Stages["implement"].TimeoutText != "3h" || cfg.Routing.Stages["review"].TimeoutText != "50m" {
			t.Fatalf("stage timeouts were not kept: %+v", cfg.Routing.Stages)
		}
		if triage := cfg.Routing.Stages["triage"]; triage.Model != "gpt-luna" || triage.Harness != "" || triage.Execution != ExecutionInProcess {
			t.Fatalf("triage route = %+v", triage)
		}
		lines := warnings.all()
		if len(lines) != 1 || !strings.Contains(lines[0], "routing.stages.triage.harness") || !strings.Contains(lines[0], "routing.stages.implement.model_tier") {
			t.Fatalf("warnings = %q", lines)
		}
		for _, value := range []string{"not a harness!", "extreme", "maximal", "carrier-pigeon", "sideways", "nonsense"} {
			if strings.Contains(lines[0], value) {
				t.Fatalf("warning %q echoes value %q", lines[0], value)
			}
		}
	})
	t.Run("aliases and merge keys keep shared control-plane anchors", func(t *testing.T) {
		data := []byte(`workspace: default
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: &control {model: gpt-luna, effort: high, timeout: 20m}
    planning: *control
  spec:
    <<: *control
    timeout: 45m
  implementation:
    <<: [*control]
    harness: codex
    timeout: 3h
review:
  seats:
    - &seat {model: gpt-terra, effort: high}
    - *seat
`)
		var warnings warningLog
		cfg, err := ParseDeployment(data, "aliases.yaml", warnings.logf)
		if err != nil {
			t.Fatal(err)
		}
		assertNoExecutorDetail(t, cfg)
		if triage := cfg.Routing.Stages["triage"]; triage.Model != "gpt-luna" || triage.Effort != "high" || triage.TimeoutText != "20m" {
			t.Fatalf("shared anchor lost its control-plane values: %+v", triage)
		}
		if planning := cfg.ExecutionSettings.ControlPlane.Planning; planning.Model != "gpt-luna" || planning.Effort != "high" {
			t.Fatalf("aliased planning settings = %+v", planning)
		}
		if cfg.Routing.Stages["spec"].TimeoutText != "45m" || cfg.Routing.Stages["implement"].TimeoutText != "3h" || len(cfg.Review.Seats) != 2 {
			t.Fatalf("merged policy values = %+v seats=%d", cfg.Routing.Stages, len(cfg.Review.Seats))
		}
		lines := warnings.all()
		if len(lines) != 1 {
			t.Fatalf("warnings = %q", lines)
		}
		for _, name := range []string{"execution_settings.spec.model", "execution_settings.spec.effort", "execution_settings.implementation.harness", "review.seats[].model"} {
			if !strings.Contains(lines[0], name) {
				t.Fatalf("warning %q does not name %s", lines[0], name)
			}
		}
		if strings.Contains(lines[0], "control_plane.triage") || strings.Contains(lines[0], "control_plane.planning") {
			t.Fatalf("warning %q names a retained control-plane field", lines[0])
		}
	})
	t.Run("unknown keys still fail", func(t *testing.T) {
		for name, data := range map[string]string{
			"top level":      "workspace: default\nharnesses: []\nunexpected_key: 1\nexecution_settings: {control_plane: {triage: {model: m}}}\n",
			"stage route":    "workspace: default\nexecution_settings: {control_plane: {triage: {model: m}}}\nrouting: {stages: {implement: {budget_usd: 3}}}\n",
			"execution keys": "workspace: default\nexecution_settings: {control_plane: {triage: {model: m}}}\nexecution: {unknown_gate: true}\n",
		} {
			if _, err := ParseDeployment([]byte(data), name, nil); err == nil {
				t.Fatalf("%s: unknown key loaded", name)
			}
		}
	})
	t.Run("control-plane triage model is required", func(t *testing.T) {
		_, err := ParseDeployment([]byte("workspace: default\nharnesses: []\n"), "missing.yaml", nil)
		if err == nil || !strings.Contains(err.Error(), "execution_settings.control_plane.triage.model is required") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no warning without retired keys", func(t *testing.T) {
		data := []byte("workspace: default\nexecution_settings: {control_plane: {triage: {model: m}}}\n")
		if _, err := ParseDeployment(data, "clean.yaml", func(format string, args ...any) {
			t.Fatalf("unexpected warning: "+format, args...)
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// component-runtime "The client loader": LoadClient keeps only client-local
// settings of the same authored YAML that the deployment loader strips to
// policy, and neither loader validates a retired executor value.
func TestDeploymentAndClientLoadHaveSeparateSemantics(t *testing.T) {
	path := writeDeploymentFixture(t, legacyInitShapedDeployment(t))
	local, err := LoadClient(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if local.ExecutionSettings != nil || len(local.Routing.Stages) != 0 || len(local.Review.Seats) != 0 || len(local.PlanningModels) != 0 {
		t.Fatalf("client loader kept execution detail: settings=%+v routes=%+v seats=%+v", local.ExecutionSettings, local.Routing.Stages, local.Review.Seats)
	}
	deployment, err := LoadDeployment(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoExecutorDetail(t, deployment)
	invalid := strings.ReplaceAll(string(legacyInitShapedDeployment(t)), "model_policy: explicit", "model_policy: bogus")
	if invalid == string(legacyInitShapedDeployment(t)) {
		t.Fatal("fixture replacement did not apply")
	}
	if _, err := LoadClient(writeDeploymentFixture(t, []byte(invalid)), nil); err != nil {
		t.Fatalf("client loader validated a retired value: %v", err)
	}
	if _, err := ParseDeployment([]byte(invalid), "invalid.yaml", nil); err != nil {
		t.Fatalf("deployment loader validated a retired value: %v", err)
	}
}

// Concurrent deployment loads share no warning state (component-runtime).
func TestDeploymentLegacyWarningsConcurrent(t *testing.T) {
	path := writeDeploymentFixture(t, legacyInitShapedDeployment(t))
	const loads = 16
	logs := make([]warningLog, loads)
	var shared warningLog
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, loads)
	for i := 0; i < loads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := LoadDeployment(path, func(format string, args ...any) {
				logs[i].logf(format, args...)
				shared.logf(format, args...)
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(shared.all()); got != loads {
		t.Fatalf("shared warnings = %d, want one per load (%d)", got, loads)
	}
	first := logs[0].all()
	for i := range logs {
		lines := logs[i].all()
		if len(lines) != 1 || lines[0] != first[0] {
			t.Fatalf("load %d warnings = %q, want %q", i, lines, first)
		}
	}
}

// conveyor init writes MarshalDeployment output: no executor key at any path,
// and the file reloads without a warning.
func TestMarshalDeploymentRoundTripsPolicyOnly(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://conveyor@db.invalid/conveyor")
	legacy, err := ParseDeployment(legacyInitShapedDeployment(t), "legacy.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalDeployment(legacy)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentYAMLHasNoExecutorDetail(t, data)
	reloaded, err := ParseDeployment(data, "rendered.yaml", func(format string, args ...any) {
		t.Fatalf("rendered deployment carried retired detail: "+format, args...)
	})
	if err != nil {
		t.Fatalf("rendered deployment does not reload: %v\n%s", err, data)
	}
	assertNoExecutorDetail(t, reloaded)
	if reloaded.Routing.Stages["triage"].Model != "gpt-5.6-luna" || reloaded.ExecutionSettings.ControlPlane.Planning.Model != "gpt-5.6-luna" {
		t.Fatalf("control-plane models were not rendered: %s", data)
	}
	if reloaded.Execution.DefaultPlanState != "needed" || reloaded.Execution.MergeApproval || len(reloaded.Review.Seats) != 1 || reloaded.MaxBounces != 7 {
		t.Fatalf("policy changed through the round trip: %s", data)
	}
	if reloaded.Routing.Stages["implement"].TimeoutText != "4h" {
		t.Fatalf("stage timeout changed through the round trip: %s", data)
	}
}

// assertDeploymentYAMLHasNoExecutorDetail walks every key path of a rendered
// deployment file. Model and effort keys are allowed only in the in-process
// control-plane settings (DEC-56(3)).
func assertDeploymentYAMLHasNoExecutorDetail(t *testing.T, data []byte) {
	t.Helper()
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	forbiddenAnywhere := map[string]bool{
		"harness": true, "harnesses": true, "setups": true, "default_setup": true, "routing": true,
		"model_policy": true, "argv": true, "command": true, "model_args": true, "effort_args": true,
		"probe_command": true, "mcp_transport": true, "fallback_model": true, "fallback_harness": true,
		"first_activity_timeout": true,
	}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				childPath := key
				if path != "" {
					childPath = path + "." + key
				}
				if forbiddenAnywhere[key] {
					t.Fatalf("rendered deployment carries %s:\n%s", childPath, data)
				}
				if (key == "model" || key == "effort" || key == "model_tier") &&
					!strings.HasPrefix(childPath, "execution_settings.control_plane.triage.") &&
					!strings.HasPrefix(childPath, "execution_settings.control_plane.planning.") {
					t.Fatalf("rendered deployment carries executor %s:\n%s", childPath, data)
				}
				walk(childPath, child)
			}
		case []any:
			for _, child := range typed {
				walk(path+"[]", child)
			}
		}
	}
	walk("", root)
}

// The annotated combined example loads on both sides: each loader ignores its
// setups and harnesses.
func TestDeploymentLoadsAnnotatedExample(t *testing.T) {
	path := filepath.Join("..", "..", "conveyor.example.yaml")
	var warnings warningLog
	cfg, err := LoadDeployment(path, warnings.logf)
	if err != nil {
		t.Fatal(err)
	}
	assertNoExecutorDetail(t, cfg)
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], "harnesses") || !strings.Contains(lines[0], "setups") {
		t.Fatalf("warnings = %q", lines)
	}
	if _, err := LoadClient(path, nil); err != nil {
		t.Fatalf("client loader refused the annotated example: %v", err)
	}
}

// deploymentPolicyView is the policy and control-plane state a deployment
// value contributes at runtime: the workspace policy it seeds, parsed the way
// RuntimeConfig parses a stored document, plus the in-process stage settings.
type deploymentPolicyView struct {
	MaxBounces     int
	QueueTimeout   string
	StageTimeouts  map[string]string
	ReviewSeats    int
	Execution      ExecutionPolicy
	Repos          []Repo
	Monitor        MonitorConfig
	Triage         StageRoute
	Planning       PlanningSettings
	PlanningModels []string
}

func policyView(t *testing.T, cfg *Config) deploymentPolicyView {
	t.Helper()
	stored, err := MarshalPolicyDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := ParseStoredWorkspaceDocument(stored, cfg, "policy view")
	if err != nil {
		t.Fatal(err)
	}
	view := deploymentPolicyView{
		MaxBounces: runtime.MaxBounces, QueueTimeout: runtime.WorkOrderQueueTimeoutText,
		StageTimeouts: map[string]string{}, ReviewSeats: len(runtime.Review.Seats),
		Execution: runtime.Execution, Repos: runtime.Repos, Monitor: runtime.Monitor,
		PlanningModels: cfg.PlanningModels,
	}
	for _, stage := range []string{"spec", "implement", "review", "verify"} {
		view.StageTimeouts[stage] = runtime.Routing.Stages[stage].TimeoutText
	}
	triage := runtime.Routing.Stages["triage"]
	view.Triage = StageRoute{Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText}
	if cfg.ExecutionSettings != nil {
		view.Planning = cfg.ExecutionSettings.ControlPlane.Planning
	}
	return view
}

// legacySetupDeployment renders a pre-DEC-56 file whose authoritative
// settings live in the named default setup. withProjection adds a stale
// top-level projection and routing that disagree with that setup.
func legacySetupDeployment(t *testing.T, withProjection bool) []byte {
	t.Helper()
	harness := legacyHarnessYAML()["name"].(string)
	stage := func(model, timeout string) ImplementationSettings {
		return ImplementationSettings{Harness: harness, Model: model, ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: timeout}
	}
	fast := ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage:   ModelTimeoutSettings{Model: "setup-triage-model", Effort: "medium", TimeoutText: "25m"},
			Planning: PlanningSettings{Model: "setup-planning-model", Effort: "low", TimeoutText: "35m", ExplorationOutputTokens: 4000},
		},
		Spec:           stage("setup-spec-model", "45m"),
		Implementation: stage("setup-implement-model", "5h"),
		Verify:         stage("setup-verify-model", "90m"),
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "2h", FallbackHarness: harness, FallbackModel: "setup-fallback-model"},
	}
	fastReview := ReviewPanel{Seats: []ReviewSeat{{Model: "seat-a"}, {Model: "seat-b", Harness: harness}, {Model: "seat-c", Effort: "high"}}}
	other := ContextualExecutionSettings{
		ControlPlane:   ControlPlaneSettings{Triage: ModelTimeoutSettings{Model: "other-triage-model", TimeoutText: "5m"}},
		Spec:           stage("other-spec-model", "11m"),
		Implementation: stage("other-implement-model", "12m"),
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "13m"},
	}
	document := Config{
		Workspace: "default", MaxBounces: 6,
		Database:  DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		Execution: ExecutionPolicy{DefaultPlanState: "not_required", MergeApproval: true, VerifyStage: true, ImplementConcurrency: 3, ReviewConcurrency: 2},
		Repos:     []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", Base: "main"}},
	}
	if withProjection {
		stale := ContextualExecutionSettings{
			ControlPlane:   ControlPlaneSettings{Triage: ModelTimeoutSettings{Model: "stale-triage-model", TimeoutText: "7m"}, Planning: PlanningSettings{Model: "stale-triage-model", TimeoutText: "7m"}},
			Spec:           stage("stale-spec-model", "10m"),
			Implementation: stage("stale-implement-model", "1h"),
			Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "20m"},
		}
		document.ExecutionSettings = &stale
		document.Review = ReviewPanel{Seats: []ReviewSeat{{Model: "stale-seat"}}}
		document.Routing = Routing{Stages: map[string]StageRoute{
			"triage":    {Model: "stale-routing-model", TimeoutText: "3m", Execution: ExecutionInProcess},
			"implement": {Model: "stale-routing-implement", Harness: harness, TimeoutText: "9m", Execution: ExecutionMCP},
		}}
	}
	return withLegacyExecutorKeys(t, document, []map[string]any{
		legacySetupYAML(t, "other", other, ReviewPanel{Seats: []ReviewSeat{{Model: "other-seat"}}}, ""),
		legacySetupYAML(t, "fast", fast, fastReview, RefreshReviewFull),
	}, "fast")
}

// component-runtime "Configuration scopes": a pre-DEC-56 file keeps every
// policy and control-plane value the client loader resolved from it,
// including values held only in its named default setup, while all executor
// detail is dropped and the warning names fields without values.
func TestDeploymentLoadKeepsLegacyDefaultSetupPolicy(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Each want is what the file declares in the setup its default_setup
	// names; a stage without a timeout takes the policy default.
	type declared struct {
		data          []byte
		stageTimeouts map[string]string
		reviewSeats   int
		triage        StageRoute
		planningModel string
	}
	fast := declared{stageTimeouts: map[string]string{"spec": "45m", "implement": "5h", "review": "2h", "verify": "90m"}, reviewSeats: 3,
		triage: StageRoute{Model: "setup-triage-model", Effort: "medium", TimeoutText: "25m"}, planningModel: "setup-planning-model"}
	setupOnly, overriding := fast, fast
	setupOnly.data, overriding.data = legacySetupDeployment(t, false), legacySetupDeployment(t, true)
	fixtures := map[string]declared{
		"setup-only default setup":                    setupOnly,
		"default setup overriding a stale projection": overriding,
		"earlier init output": {data: legacyInitShapedDeployment(t), stageTimeouts: map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"}, reviewSeats: 1,
			triage: StageRoute{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"}, planningModel: "gpt-5.6-luna"},
		"annotated example": {data: example, stageTimeouts: map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"}, reviewSeats: 2,
			triage: StageRoute{Model: "gpt-5.6-luna", TimeoutText: "20m"}, planningModel: "gpt-5.6-luna"},
	}
	for name, want := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := writeDeploymentFixture(t, want.data)
			var warnings warningLog
			deployment, err := LoadDeployment(path, warnings.logf)
			if err != nil {
				t.Fatalf("deployment loader refused a previously valid file: %v", err)
			}
			assertNoExecutorDetail(t, deployment)
			got := policyView(t, deployment)
			if !reflect.DeepEqual(got.StageTimeouts, want.stageTimeouts) || got.ReviewSeats != want.reviewSeats || !reflect.DeepEqual(got.Triage, want.triage) || got.Planning.Model != want.planningModel {
				t.Fatalf("policy changed:\n got timeouts=%v seats=%d triage=%+v planning=%q\nwant timeouts=%v seats=%d triage=%+v planning=%q",
					got.StageTimeouts, got.ReviewSeats, got.Triage, got.Planning.Model, want.stageTimeouts, want.reviewSeats, want.triage, want.planningModel)
			}
			lines := warnings.all()
			if len(lines) != 1 || !strings.Contains(lines[0], "setups") || !strings.Contains(lines[0], "default_setup") {
				t.Fatalf("warnings = %q", lines)
			}
			for _, value := range []string{"setup-spec-model", "setup-fallback-model", "seat-a", "stale-spec-model", "fast", "codex", "gpt-5.6-sol"} {
				if strings.Contains(strings.TrimPrefix(lines[0], "config: "+path+":"), value) {
					t.Fatalf("warning %q echoes value %q", lines[0], value)
				}
			}
		})
	}
	t.Run("default setup values are the ones kept", func(t *testing.T) {
		deployment, err := ParseDeployment(legacySetupDeployment(t, true), "fast.yaml", nil)
		if err != nil {
			t.Fatal(err)
		}
		view := policyView(t, deployment)
		if view.StageTimeouts["spec"] != "45m" || view.StageTimeouts["implement"] != "5h" || view.StageTimeouts["review"] != "2h" || view.StageTimeouts["verify"] != "90m" || view.ReviewSeats != 3 {
			t.Fatalf("default setup timeouts or seat count lost: %+v", view)
		}
		if view.Triage.Model != "setup-triage-model" || view.Triage.Effort != "medium" || view.Triage.TimeoutText != "25m" || view.Planning.Model != "setup-planning-model" || view.Planning.ExplorationOutputTokens != 4000 {
			t.Fatalf("default setup control-plane settings lost: triage=%+v planning=%+v", view.Triage, view.Planning)
		}
		if view.MaxBounces != 6 || view.Execution.DefaultPlanState != "not_required" || !view.Execution.MergeApproval || !view.Execution.VerifyStage || view.Execution.ImplementConcurrency != 3 || view.Execution.ReviewConcurrency != 2 {
			t.Fatalf("top-level gates or bounce limit changed: %+v", view)
		}
	})
}

// req-security-boundaries REQ-2: the rendered file names the backend and never
// the URL, whatever the caller passes, and the loader restores the URL from the
// environment, so the daemon reaches the backend the file names.
func TestMarshalDeploymentOmitsDatabaseURL(t *testing.T) {
	const password = "fixture-render-pw-71c"
	for _, tc := range []struct{ raw, backend string }{
		{"postgres://fixture:" + password + "@db.invalid/conveyor", "postgres"},
		{"singlestore://fixture:" + password + "@db.invalid/conveyor", "singlestore"},
		{"fixture:" + password + "@tcp(db.invalid:3306)/conveyor", "singlestore"},
	} {
		rendered, err := MarshalDeployment(&Config{Database: DatabaseForURL(tc.raw)})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(rendered), password) || strings.Contains(string(rendered), "url:") {
			t.Fatalf("%s: rendered file holds the URL or its key", tc.backend)
		}
		var document struct {
			Database map[string]any `yaml:"database"`
		}
		if err = yaml.Unmarshal(rendered, &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Database) != 1 || document.Database["backend"] != tc.backend {
			t.Fatalf("%s: database node has %d keys, want backend only", tc.backend, len(document.Database))
		}
		holds, err := DeploymentHoldsDatabaseURL(rendered)
		if err != nil || holds {
			t.Fatalf("%s: holds=%t err=%v", tc.backend, holds, err)
		}
	}
}

func TestDeploymentHoldsDatabaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		want           bool
	}{
		{"no database", "workspace: demo\n", false},
		{"backend only", "database: {backend: postgres}\n", false},
		{"empty url", "database: {backend: postgres, url: \"\"}\n", false},
		{"url", "database: {url: \"postgres://fixture:fixture-pw@db.invalid/conveyor\"}\n", true},
		{"url through an alias", "shared: &shared {url: \"postgres://fixture:fixture-pw@db.invalid/conveyor\"}\ndatabase: *shared\n", true},
		{"url through a merge key", "shared: &shared {url: \"postgres://fixture:fixture-pw@db.invalid/conveyor\"}\ndatabase: {<<: *shared}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeploymentHoldsDatabaseURL([]byte(tc.document))
			if err != nil || got != tc.want {
				t.Fatalf("got %t err=%v, want %t", got, err, tc.want)
			}
		})
	}
	_, err := DeploymentHoldsDatabaseURL([]byte("database: {url: [\"postgres://fixture:fixture-pw@db.invalid/conveyor\"]}\n"))
	if err == nil {
		t.Fatal("a url that is not a string must fail")
	}
	if strings.Contains(err.Error(), "fixture-pw") {
		t.Fatal("the error repeats the document's password")
	}
}
