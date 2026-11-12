package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// legacyInitShapedDeployment renders the deployment file that conveyor init
// wrote before DEC-56: control-plane models plus a full executor setup.
func legacyInitShapedDeployment(t *testing.T) []byte {
	t.Helper()
	settings := legacyExecutionSettingsYAML(
		map[string]any{
			"triage":   map[string]any{"model": "gpt-5.6-luna", "effort": "high", "timeout": "20m"},
			"planning": map[string]any{"model": "gpt-5.6-luna", "effort": "high", "timeout": "20m"},
		},
		map[string]any{
			"spec":           legacyStageYAML("gpt-5.6-sol", "30m"),
			"implementation": legacyStageYAML("gpt-5.6-sol", "4h"),
			"review":         map[string]any{"execution": "mcp", "timeout": "1h"},
		})
	review := legacyReviewYAML(map[string]any{"model": "gpt-5.6-terra", "harness": "codex", "effort": "high"})
	return withLegacyExecutorKeys(t, Config{
		Workspace: "default", MaxBounces: 7,
		WorkOrderQueueTimeoutText: DefaultWorkOrderQueueTimeoutText,
		Database:                  DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		Execution:                 ExecutionPolicy{DefaultPlanState: "needed", MergeApproval: false, ImplementConcurrency: 2, ReviewConcurrency: 1, FirstActivityTimeoutText: DefaultFirstActivityTimeoutText},
		Repos:                     []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", GitHub: "acme/conveyor", Base: "trunk", InstallConveyor: InstallSwitch(true)}},
		Monitor:                   MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	}, map[string]any{"execution_settings": settings, "review": review},
		[]map[string]any{legacySetupYAML("default", settings, review, RefreshReviewDelta)}, "default")
}

// legacyStageYAML is one agent stage's settings as earlier files wrote them,
// with the harness, model, model policy, and effort keys this release removes.
func legacyStageYAML(model, timeout string) map[string]any {
	return map[string]any{"harness": "codex", "model": model, "model_policy": "explicit", "effort": "high", "timeout": timeout}
}

// legacyExecutionSettingsYAML assembles an execution_settings block from its
// control-plane settings and its agent stages.
func legacyExecutionSettingsYAML(controlPlane map[string]any, stages map[string]any) map[string]any {
	settings := map[string]any{"control_plane": controlPlane}
	for name, stage := range stages {
		settings[name] = stage
	}
	return settings
}

// legacyReviewYAML is a review block whose seats still hold the model,
// harness, and effort keys this release removes.
func legacyReviewYAML(seats ...map[string]any) map[string]any {
	entries := make([]any, len(seats))
	for i, seat := range seats {
		entries[i] = seat
	}
	return map[string]any{"seats": entries}
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
func legacySetupYAML(name string, settings, review map[string]any, refresh string) map[string]any {
	setup := map[string]any{"name": name, "execution_settings": settings, "review": review}
	if refresh != "" {
		setup["refresh_review"] = refresh
	}
	return setup
}

// withLegacyExecutorKeys adds the retired harnesses, setups, and
// default_setup keys to a marshaled document, after replacing its top-level
// keys with the authored YAML in legacy.
func withLegacyExecutorKeys(t *testing.T, document Config, legacy map[string]any, setups []map[string]any, defaultSetup string) []byte {
	t.Helper()
	root, ok := yamlValue(t, document).(map[string]any)
	if !ok {
		t.Fatal("document did not marshal to a mapping")
	}
	for key, value := range legacy {
		root[key] = value
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
		if route := cfg.Routing.Stages[stage]; route.Execution != ExecutionMCP {
			t.Fatalf("deployment %s route execution = %q, want mcp", stage, route.Execution)
		}
	}
	if _, ok := cfg.Routing.Stages["triage"]; ok {
		t.Fatalf("deployment kept triage among the agent stage routes: %+v", cfg.Routing.Stages)
	}
	if cfg.ExecutionSettings == nil {
		t.Fatal("deployment lost its control-plane settings")
	}
	// The stage, route, and seat types hold no executor detail; the
	// pre-v1.34 control-plane spec settings are the one typed place left.
	if spec := cfg.ExecutionSettings.ControlPlane.Spec; spec.Model != "" || spec.Effort != "" {
		t.Fatalf("deployment kept control-plane spec executor settings: %+v", spec)
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
		triage := cfg.Routing.Triage
		if triage.Model != "gpt-5.6-luna" || triage.Effort != "high" || triage.TimeoutText != "20m" || triage.Timeout != 20*time.Minute {
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
		if triage := cfg.Routing.Triage; triage.Model != "gpt-luna" || triage.Effort != "" {
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
		if triage := cfg.Routing.Triage; triage.Model != "gpt-luna" || triage.Effort != "high" || triage.TimeoutText != "20m" {
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
	if reloaded.Routing.Triage.Model != "gpt-5.6-luna" || reloaded.ExecutionSettings.ControlPlane.Planning.Model != "gpt-5.6-luna" {
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

// The annotated example shows only supported deployment settings: it loads
// without a retired-key warning, states no agent harness, model, or effort
// anywhere, and keeps its policy and control-plane settings (DEC-132).
func TestDeploymentLoadsAnnotatedExample(t *testing.T) {
	path := filepath.Join("..", "..", "conveyor.example.yaml")
	var warnings warningLog
	cfg, err := LoadDeployment(path, warnings.logf)
	if err != nil {
		t.Fatal(err)
	}
	assertNoExecutorDetail(t, cfg)
	if lines := warnings.all(); len(lines) != 0 {
		t.Fatalf("the example carries retired execution detail: %q", lines)
	}
	example, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentYAMLHasNoExecutorDetail(t, exampleWithoutFirstActivityTimeout(t, example))
	view := policyView(t, cfg)
	if !reflect.DeepEqual(view.StageTimeouts, map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"}) || view.ReviewSeats != 2 ||
		view.Triage != (TriageRoute{Model: "gpt-5.6-luna", TimeoutText: "20m"}) || view.Planning.Model != "gpt-5.6-luna" {
		t.Fatalf("example policy = %+v", view)
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
	Triage         TriageRoute
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
	triage := runtime.Routing.Triage
	view.Triage = TriageRoute{Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText}
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
	fast := legacyExecutionSettingsYAML(
		map[string]any{
			"triage":   map[string]any{"model": "setup-triage-model", "effort": "medium", "timeout": "25m"},
			"planning": map[string]any{"model": "setup-planning-model", "effort": "low", "timeout": "35m", "exploration_output_tokens": 4000},
		},
		map[string]any{
			"spec":           legacyStageYAML("setup-spec-model", "45m"),
			"implementation": legacyStageYAML("setup-implement-model", "5h"),
			"verify":         legacyStageYAML("setup-verify-model", "90m"),
			"review":         map[string]any{"execution": "mcp", "timeout": "2h", "fallback_harness": "codex", "fallback_model": "setup-fallback-model"},
		})
	fastReview := legacyReviewYAML(map[string]any{"model": "seat-a"}, map[string]any{"model": "seat-b", "harness": "codex"}, map[string]any{"model": "seat-c", "effort": "high"})
	other := legacyExecutionSettingsYAML(
		map[string]any{"triage": map[string]any{"model": "other-triage-model", "timeout": "5m"}},
		map[string]any{
			"spec":           legacyStageYAML("other-spec-model", "11m"),
			"implementation": legacyStageYAML("other-implement-model", "12m"),
			"review":         map[string]any{"execution": "mcp", "timeout": "13m"},
		})
	document := Config{
		Workspace: "default", MaxBounces: 6,
		Database:  DatabaseForURL("postgres://conveyor@db.invalid/conveyor"),
		Execution: ExecutionPolicy{DefaultPlanState: "not_required", MergeApproval: true, VerifyStage: true, ImplementConcurrency: 3, ReviewConcurrency: 2},
		Repos:     []Repo{{Name: "conveyor", URL: "https://github.com/acme/conveyor.git", Base: "main"}},
	}
	legacy := map[string]any{}
	if withProjection {
		legacy["execution_settings"] = legacyExecutionSettingsYAML(
			map[string]any{
				"triage":   map[string]any{"model": "stale-triage-model", "timeout": "7m"},
				"planning": map[string]any{"model": "stale-triage-model", "timeout": "7m"},
			},
			map[string]any{
				"spec":           legacyStageYAML("stale-spec-model", "10m"),
				"implementation": legacyStageYAML("stale-implement-model", "1h"),
				"review":         map[string]any{"execution": "mcp", "timeout": "20m"},
			})
		legacy["review"] = legacyReviewYAML(map[string]any{"model": "stale-seat"})
		legacy["routing"] = map[string]any{"stages": map[string]any{
			"triage":    map[string]any{"model": "stale-routing-model", "timeout": "3m", "execution": "in_process"},
			"implement": map[string]any{"model": "stale-routing-implement", "harness": "codex", "timeout": "9m", "execution": "mcp"},
		}}
	}
	return withLegacyExecutorKeys(t, document, legacy, []map[string]any{
		legacySetupYAML("other", other, legacyReviewYAML(map[string]any{"model": "other-seat"}), ""),
		legacySetupYAML("fast", fast, fastReview, RefreshReviewFull),
	}, "fast")
}

// component-runtime "Configuration scopes": a pre-DEC-56 file keeps every
// policy and control-plane value the client loader resolved from it,
// including values held only in its named default setup, while all executor
// detail is dropped and the warning names fields without values.
func TestDeploymentLoadKeepsLegacyDefaultSetupPolicy(t *testing.T) {
	// Each want is what the file declares in the setup its default_setup
	// names; a stage without a timeout takes the policy default.
	type declared struct {
		data          []byte
		stageTimeouts map[string]string
		reviewSeats   int
		triage        TriageRoute
		planningModel string
	}
	fast := declared{stageTimeouts: map[string]string{"spec": "45m", "implement": "5h", "review": "2h", "verify": "90m"}, reviewSeats: 3,
		triage: TriageRoute{Model: "setup-triage-model", Effort: "medium", TimeoutText: "25m"}, planningModel: "setup-planning-model"}
	setupOnly, overriding := fast, fast
	setupOnly.data, overriding.data = legacySetupDeployment(t, false), legacySetupDeployment(t, true)
	fixtures := map[string]declared{
		"setup-only default setup":                    setupOnly,
		"default setup overriding a stale projection": overriding,
		"earlier init output": {data: legacyInitShapedDeployment(t), stageTimeouts: map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"}, reviewSeats: 1,
			triage: TriageRoute{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"}, planningModel: "gpt-5.6-luna"},
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

// everyRemovedKeyDeployment is a deployment file that carries every
// agent-stage key this release removed from the configuration types beside
// its policy and control-plane settings, most with values the earlier
// validation would refuse (component-runtime invariant 13).
const everyRemovedKeyDeployment = `workspace: default
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    planning: {model: deploy-planner, timeout: 9m}
    spec: {model: "removed spec!", effort: extreme, timeout: 31m}
  spec: {harness: "bad harness!", model: "", model_policy: bogus, effort: extreme, timeout: 32m}
  implementation: {harness: missing, model: removed-impl, model_policy: explicit, effort: maximal, timeout: 3h}
  verify: {harness: missing, model: removed-verify, model_policy: harness_default, effort: high, timeout: 75m}
  review: {execution: sideways, timeout: 55m, fallback_model: removed-fallback, fallback_harness: missing}
routing:
  stages:
    triage: {model: route-triage, model_tier: route-tier, effort: low, timeout: 9m, execution: mcp, harness: missing}
    implement: {model: removed-route, model_policy: nonsense, harness: missing, effort: extreme, harnesses: [a, b], model_tier: big, timeout: 7h, execution: in_process}
    review: {model: removed-review, harness: missing, effort: high, timeout: 65m, execution: in_process}
review:
  distinct_models: true
  seats:
    - {model: removed-seat, harness: missing, effort: extreme}
    - {model: ""}
    - {}
planning_models: [deploy-planner]
`

// A deployment file with every removed key loads, names each retired key
// once without its value, and resolves the stage timeouts, seat count,
// distinct_models, and triage settings the earlier loader resolved; the
// legacy triage route spellings still supply the triage model and effort
// (component-runtime invariant 13; DEC-132(3)).
func TestDeploymentWithEveryRemovedKeyLoadsAndLogsNames(t *testing.T) {
	var warnings warningLog
	cfg, err := ParseDeployment([]byte(everyRemovedKeyDeployment), "every-key.yaml", warnings.logf)
	if err != nil {
		t.Fatal(err)
	}
	assertNoExecutorDetail(t, cfg)
	// Stage timeouts come from execution_settings first, then the routes,
	// as before; the route's own 7h implement timeout yields to 3h.
	for stage, want := range map[string]string{"spec": "32m", "implement": "3h", "review": "55m", "verify": "75m"} {
		if got := cfg.Routing.Stages[stage]; got.TimeoutText != want || got.Execution != ExecutionMCP {
			t.Fatalf("%s route = %+v, want timeout %s over MCP", stage, got, want)
		}
	}
	if len(cfg.Review.Seats) != 3 || !cfg.Review.DistinctModels {
		t.Fatalf("review = %+v, want three seats with distinct models", cfg.Review)
	}
	if want := (TriageRoute{Model: "route-triage", Effort: "low", TimeoutText: "9m", Timeout: 9 * time.Minute}); cfg.Routing.Triage != want {
		t.Fatalf("triage route = %+v, want %+v", cfg.Routing.Triage, want)
	}
	if triage := cfg.ExecutionSettings.ControlPlane.Triage; triage != (ModelTimeoutSettings{Model: "route-triage", Effort: "low", TimeoutText: "9m"}) {
		t.Fatalf("control-plane triage = %+v", triage)
	}
	if planning := cfg.ExecutionSettings.ControlPlane.Planning; planning.Model != "deploy-planner" || planning.TimeoutText != "9m" {
		t.Fatalf("planning = %+v", planning)
	}
	lines := warnings.all()
	if len(lines) != 1 {
		t.Fatalf("warnings = %q, want one line", lines)
	}
	for _, name := range []string{
		"execution_settings.control_plane.spec.model", "execution_settings.control_plane.spec.effort",
		"execution_settings.spec.harness", "execution_settings.spec.model", "execution_settings.spec.model_policy", "execution_settings.spec.effort",
		"execution_settings.implementation.harness", "execution_settings.implementation.model", "execution_settings.implementation.model_policy", "execution_settings.implementation.effort",
		"execution_settings.verify.harness", "execution_settings.verify.model", "execution_settings.verify.model_policy", "execution_settings.verify.effort",
		"execution_settings.review.execution", "execution_settings.review.fallback_model", "execution_settings.review.fallback_harness",
		"routing.stages.triage.execution", "routing.stages.triage.harness",
		"routing.stages.implement.model", "routing.stages.implement.model_policy", "routing.stages.implement.harness",
		"routing.stages.implement.effort", "routing.stages.implement.harnesses", "routing.stages.implement.model_tier", "routing.stages.implement.execution",
		"routing.stages.review.model", "routing.stages.review.harness", "routing.stages.review.effort", "routing.stages.review.execution",
		"review.seats[].model", "review.seats[].harness", "review.seats[].effort",
	} {
		if !strings.Contains(lines[0], name) {
			t.Fatalf("warning %q does not name %s", lines[0], name)
		}
	}
	// The triage spellings still apply, so they are moved, not retired.
	for _, adapted := range []string{"routing.stages.triage.model", "routing.stages.triage.effort"} {
		if strings.Contains(lines[0], adapted) {
			t.Fatalf("warning %q names %s, whose value still applies", lines[0], adapted)
		}
	}
	for _, value := range []string{"removed spec!", "extreme", "bad harness!", "bogus", "removed-impl", "maximal", "removed-verify", "sideways",
		"removed-fallback", "route-triage", "route-tier", "removed-route", "nonsense", "missing", "removed-review", "removed-seat"} {
		if strings.Contains(lines[0], value) {
			t.Fatalf("warning %q echoes value %q", lines[0], value)
		}
	}
}

// The legacy triage route keeps its earlier precedence: the control-plane
// triage settings win, the route's model comes next, and its model_tier
// alias supplies the model only when the route names none (DEC-132(3)).
func TestDeploymentLegacyTriageRoutePrecedence(t *testing.T) {
	for name, test := range map[string]struct {
		document, model, effort string
	}{
		"control plane wins": {
			document: "execution_settings: {control_plane: {triage: {model: control-model, effort: high}}}\nrouting: {stages: {triage: {model: route-model, model_tier: route-tier, effort: low}}}\n",
			model:    "control-model", effort: "high",
		},
		"route model": {
			document: "routing: {stages: {triage: {model: route-model, model_tier: route-tier, effort: low}}}\n",
			model:    "route-model", effort: "low",
		},
		"model tier alias": {
			document: "routing: {stages: {triage: {model_tier: route-tier}}}\n",
			model:    "route-tier",
		},
		"blank control-plane model takes the route": {
			document: "execution_settings: {control_plane: {triage: {model: \"\", effort: high}}}\nrouting: {stages: {triage: {model: route-model, effort: low}}}\n",
			model:    "route-model", effort: "high",
		},
		"route shared with an agent stage through an anchor": {
			document: "routing: {stages: {triage: &shared {model: anchored-model, effort: minimal, timeout: 4m}, spec: *shared}}\n",
			model:    "anchored-model", effort: "minimal",
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := "workspace: default\n" + test.document
			var warnings warningLog
			cfg, err := ParseDeployment([]byte(data), name+".yaml", warnings.logf)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Routing.Triage.Model != test.model || cfg.Routing.Triage.Effort != test.effort {
				t.Fatalf("triage route = %+v, want model %q effort %q", cfg.Routing.Triage, test.model, test.effort)
			}
			if got := cfg.ExecutionSettings.ControlPlane.Triage; got.Model != test.model || got.Effort != test.effort {
				t.Fatalf("control-plane triage = %+v", got)
			}
			for _, line := range warnings.all() {
				if strings.Contains(line, "routing.stages.triage.model") || strings.Contains(line, "routing.stages.triage.effort") {
					t.Fatalf("warning %q retires a triage spelling that still applies", line)
				}
			}
			if strings.Contains(test.document, "spec: *shared") {
				lines := warnings.all()
				if cfg.Routing.Stages["spec"].TimeoutText != "4m" || len(lines) != 1 || !strings.Contains(lines[0], "routing.stages.spec.model") || !strings.Contains(lines[0], "routing.stages.spec.effort") {
					t.Fatalf("aliased spec route = %+v warnings=%q", cfg.Routing.Stages["spec"], lines)
				}
			}
		})
	}
}

// exampleWithoutFirstActivityTimeout drops the example's first_activity_timeout,
// a policy key the rendered-file walk refuses only because conveyor init
// never writes it.
func exampleWithoutFirstActivityTimeout(t *testing.T, example []byte) []byte {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal(example, &root); err != nil {
		t.Fatal(err)
	}
	if execution, ok := root["execution"].(map[string]any); ok {
		delete(execution, "first_activity_timeout")
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
