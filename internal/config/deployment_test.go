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
	harness := HarnessTemplates()[0].Harness
	settings := ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage:   ModelTimeoutSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
			Planning: PlanningSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
		},
		Spec:           ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: "30m"},
		Implementation: ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: "4h"},
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "1h"},
	}
	review := ReviewPanel{Seats: []ReviewSeat{{Model: "gpt-5.6-terra", Harness: harness.Name, Effort: "high"}}}
	data, err := yaml.Marshal(Config{
		Workspace: "default", MaxBounces: 7,
		WorkOrderQueueTimeoutText: DefaultWorkOrderQueueTimeoutText,
		Database:                  DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		ExecutionSettings:         &settings,
		Harnesses:                 []Harness{harness},
		Review:                    review,
		Setups:                    []ExecutionSetup{{Name: "default", ExecutionSettings: settings, Review: review, RefreshReview: RefreshReviewDelta}},
		DefaultSetup:              "default",
		Execution:                 ExecutionPolicy{SpecApproval: true, MergeApproval: false, ImplementConcurrency: 2, ReviewConcurrency: 1, FirstActivityTimeoutText: DefaultFirstActivityTimeoutText},
		Repos:                     []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", GitHub: "acme/conveyor", Base: "trunk", InstallConveyor: InstallSwitch(true)}},
		Monitor:                   MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
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
	if cfg.Harnesses != nil || cfg.Setups != nil || cfg.DefaultSetup != "" {
		t.Fatalf("deployment kept execution setup state: harnesses=%v setups=%v default=%q", cfg.Harnesses, cfg.Setups, cfg.DefaultSetup)
	}
	for _, stage := range []string{"spec", "implement", "review", "verify"} {
		route := cfg.Routing.Stages[stage]
		if route.Model != "" || route.Harness != "" || route.Effort != "" || route.ModelPolicy != "" || route.EffectiveModel != "" {
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
		if !cfg.Execution.SpecApproval || cfg.Execution.MergeApproval || cfg.Execution.ImplementConcurrency != 2 || cfg.Execution.ReviewConcurrency != 1 {
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

// component-harness-execution "The local execution document": the client
// loader keeps and validates the same authored YAML that the deployment
// loader strips.
func TestDeploymentAndLocalLoadHaveSeparateSemantics(t *testing.T) {
	path := writeDeploymentFixture(t, legacyInitShapedDeployment(t))
	local, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Harnesses) != 1 || local.Harnesses[0].Name != "codex" || len(local.Setups) != 1 || local.DefaultSetup != "default" {
		t.Fatalf("client loader lost local execution setup: harnesses=%+v setups=%+v", local.Harnesses, local.Setups)
	}
	if local.Routing.Stages["implement"].Model != "gpt-5.6-sol" || local.Review.Seats[0].Model != "gpt-5.6-terra" {
		t.Fatalf("client loader lost models: %+v seats=%+v", local.Routing.Stages["implement"], local.Review.Seats)
	}
	deployment, err := LoadDeployment(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoExecutorDetail(t, deployment)
	// A client-local invalid value still fails the client loader.
	invalid := strings.ReplaceAll(string(legacyInitShapedDeployment(t)), "model_policy: explicit", "model_policy: bogus")
	if invalid == string(legacyInitShapedDeployment(t)) {
		t.Fatal("fixture replacement did not apply")
	}
	if _, err := Load(writeDeploymentFixture(t, []byte(invalid))); err == nil {
		t.Fatal("client loader accepted an invalid model policy")
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
	if !reloaded.Execution.SpecApproval || reloaded.Execution.MergeApproval || len(reloaded.Review.Seats) != 1 || reloaded.MaxBounces != 7 {
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

// The annotated combined example loads on both sides: the client loader keeps
// its setups and the deployment loader ignores them.
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
	if _, err := Load(path); err != nil {
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
	harness := HarnessTemplates()[0].Harness
	stage := func(model, timeout string) ImplementationSettings {
		return ImplementationSettings{Harness: harness.Name, Model: model, ModelPolicy: ModelPolicyExplicit, Effort: "high", TimeoutText: timeout}
	}
	fast := ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage:   ModelTimeoutSettings{Model: "setup-triage-model", Effort: "medium", TimeoutText: "25m"},
			Planning: PlanningSettings{Model: "setup-planning-model", Effort: "low", TimeoutText: "35m", ExplorationOutputTokens: 4000},
		},
		Spec:           stage("setup-spec-model", "45m"),
		Implementation: stage("setup-implement-model", "5h"),
		Verify:         stage("setup-verify-model", "90m"),
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "2h", FallbackHarness: harness.Name, FallbackModel: "setup-fallback-model"},
	}
	fastReview := ReviewPanel{Seats: []ReviewSeat{{Model: "seat-a"}, {Model: "seat-b", Harness: harness.Name}, {Model: "seat-c", Effort: "high"}}}
	other := ContextualExecutionSettings{
		ControlPlane:   ControlPlaneSettings{Triage: ModelTimeoutSettings{Model: "other-triage-model", TimeoutText: "5m"}},
		Spec:           stage("other-spec-model", "11m"),
		Implementation: stage("other-implement-model", "12m"),
		Review:         ReviewExecutionSettings{Execution: ExecutionMCP, TimeoutText: "13m"},
	}
	document := Config{
		Workspace: "default", MaxBounces: 6,
		Database:     DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		Harnesses:    []Harness{harness},
		Setups:       []ExecutionSetup{{Name: "other", ExecutionSettings: other, Review: ReviewPanel{Seats: []ReviewSeat{{Model: "other-seat"}}}}, {Name: "fast", ExecutionSettings: fast, Review: fastReview, RefreshReview: RefreshReviewFull}},
		DefaultSetup: "fast",
		Execution:    ExecutionPolicy{SpecApproval: false, MergeApproval: true, VerifyStage: true, ImplementConcurrency: 3, ReviewConcurrency: 2},
		Repos:        []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", Base: "main"}},
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
			"implement": {Model: "stale-routing-implement", Harness: harness.Name, TimeoutText: "9m", Execution: ExecutionMCP},
		}}
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
	fixtures := map[string][]byte{
		"setup-only default setup":                    legacySetupDeployment(t, false),
		"default setup overriding a stale projection": legacySetupDeployment(t, true),
		"earlier init output":                         legacyInitShapedDeployment(t),
		"annotated example":                           example,
	}
	for name, data := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := writeDeploymentFixture(t, data)
			local, err := Load(path)
			if err != nil {
				t.Fatalf("fixture is not a valid pre-DEC-56 file: %v", err)
			}
			var warnings warningLog
			deployment, err := LoadDeployment(path, warnings.logf)
			if err != nil {
				t.Fatalf("deployment loader refused a previously valid file: %v", err)
			}
			assertNoExecutorDetail(t, deployment)
			want, got := policyView(t, local), policyView(t, deployment)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("policy changed:\n got %+v\nwant %+v", got, want)
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
		if view.MaxBounces != 6 || view.Execution.SpecApproval || !view.Execution.MergeApproval || !view.Execution.VerifyStage || view.Execution.ImplementConcurrency != 3 || view.Execution.ReviewConcurrency != 2 {
			t.Fatalf("top-level gates or bounce limit changed: %+v", view)
		}
	})
}
