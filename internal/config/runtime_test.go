package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// runtimeTestDeployment is a deployment file whose control-plane values all
// differ from the workspace documents below (component-runtime).
const runtimeTestDeployment = `workspace: demo
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: deploy-triage, effort: low, timeout: 7m}
    planning:
      model: deploy-planner
      effort: high
      timeout: 9m
      exploration_output_tokens: 1234
      context: {depth: 3, nodes: 11, renderable_bytes: 4096, artifact_refs: 5, authority_nodes: 9}
  implementation: {timeout: 3h}
planning_models: [deploy-planner, deploy-alternate]
repos:
  - {name: conveyor, url: "https://github.com/example/conveyor", base: main}
`

func runtimeDeployment(t *testing.T) *Config {
	t.Helper()
	deployment, err := ParseDeployment([]byte(runtimeTestDeployment), "runtime.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	return deployment
}

// runtimePolicyDocument is a stored policy-only document with workspace policy
// distinct from the deployment's.
func runtimePolicyDocument(t *testing.T, maxBounces int, specTimeout string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`workspace: demo
max_bounces: %d
work_order_queue_timeout: 12h
stage_timeouts: {spec: %s, implement: 5h, review: 45m, verify: 50m}
review: {seats: [{}, {}]}
execution: {first_activity_timeout: 2m, spec_approval: true}
repos:
  - {name: workspace-repo, url: "https://github.com/example/workspace-repo", base: trunk}
monitor: {enabled: true, repositories: [workspace-repo]}
`, maxBounces, specTimeout))
}

// runtimeLegacyDocument is a pre-DEC-56 stored document whose own
// control-plane settings and allowlist must not win over the deployment's.
func runtimeLegacyDocument(t *testing.T) []byte {
	t.Helper()
	seed, err := normalize(validConfig(), "legacy seed")
	if err != nil {
		t.Fatal(err)
	}
	route := seed.Routing.Stages["triage"]
	route.Model, route.Effort, route.TimeoutText = "legacy-triage", "minimal", "30m"
	seed.Routing.Stages["triage"] = route
	seed.ExecutionSettings.ControlPlane.Planning.Model = "legacy-planner"
	seed.ExecutionSettings.ControlPlane.Planning.TimeoutText = "15m"
	seed.PlanningModels = []string{"legacy-planner"}
	data, err := MarshalWorkspaceDocument(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "legacy-planner") || !strings.Contains(string(data), "legacy-triage") {
		t.Fatalf("legacy fixture lacks its own control-plane values:\n%s", data)
	}
	return data
}

func assertDeploymentControlPlane(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.ExecutionSettings == nil {
		t.Fatal("runtime configuration has no control-plane settings")
	}
	planning := cfg.ExecutionSettings.ControlPlane.Planning
	want := PlanningSettings{
		Model: "deploy-planner", Effort: "high", TimeoutText: "9m", ExplorationOutputTokens: 1234,
		Context: LineageContextSettings{Depth: 3, Nodes: 11, RenderableBytes: 4096, ArtifactRefs: 5, AuthorityNodes: 9},
	}
	if planning != want {
		t.Fatalf("planning settings = %+v, want %+v", planning, want)
	}
	triage := cfg.ExecutionSettings.ControlPlane.Triage
	if triage != (ModelTimeoutSettings{Model: "deploy-triage", Effort: "low", TimeoutText: "7m"}) {
		t.Fatalf("triage settings = %+v", triage)
	}
	if cfg.ExecutionSettings.ControlPlane.Spec != (ModelTimeoutSettings{}) {
		t.Fatalf("legacy control-plane spec was composed: %+v", cfg.ExecutionSettings.ControlPlane.Spec)
	}
	if !slices.Equal(cfg.PlanningModels, []string{"deploy-planner", "deploy-alternate"}) {
		t.Fatalf("planning allowlist = %v", cfg.PlanningModels)
	}
	route := cfg.Routing.Stages["triage"]
	if route.Model != "deploy-triage" || route.Effort != "low" || route.TimeoutText != "7m" || route.Timeout != 7*time.Minute || route.Execution != ExecutionInProcess {
		t.Fatalf("triage route = %+v", route)
	}
}

func TestRuntimeWorkspaceConfigUsesDeploymentControlPlane(t *testing.T) {
	t.Run("policy-only document", func(t *testing.T) {
		deployment := runtimeDeployment(t)
		data := runtimePolicyDocument(t, 6, "25m")
		raw, _, err := ParseStoredWorkspaceDocument(data, deployment, "raw policy")
		if err != nil {
			t.Fatal(err)
		}
		if raw.ExecutionSettings != nil || raw.PlanningModels != nil {
			t.Fatalf("raw policy parsing gained settings=%+v allowlist=%v", raw.ExecutionSettings, raw.PlanningModels)
		}
		cfg, err := ParseRuntimeWorkspaceDocument(data, deployment, "runtime policy")
		if err != nil {
			t.Fatal(err)
		}
		assertDeploymentControlPlane(t, cfg)
		if cfg.Workspace != "demo" || cfg.MaxBounces != 6 || cfg.WorkOrderQueueTimeoutText != "12h" || !cfg.Execution.SpecApproval {
			t.Fatalf("workspace policy lost: workspace=%q bounces=%d queue=%q gates=%+v", cfg.Workspace, cfg.MaxBounces, cfg.WorkOrderQueueTimeoutText, cfg.Execution)
		}
		if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "workspace-repo" || cfg.Repos[0].Base != "trunk" || !cfg.Monitor.Enabled || !slices.Equal(cfg.Monitor.Repositories, []string{"workspace-repo"}) {
			t.Fatalf("workspace repositories or monitor replaced: repos=%+v monitor=%+v", cfg.Repos, cfg.Monitor)
		}
		if len(cfg.Review.Seats) != 2 {
			t.Fatalf("review seats = %d", len(cfg.Review.Seats))
		}
		for stage, timeout := range map[string]string{"spec": "25m", "implement": "5h", "review": "45m", "verify": "50m"} {
			route := cfg.Routing.Stages[stage]
			if route.TimeoutText != timeout || route.Model != "" || route.Harness != "" || route.Effort != "" || route.ModelPolicy != "" {
				t.Fatalf("%s route = %+v", stage, route)
			}
		}
		settings := cfg.ExecutionSettings
		for name, stage := range map[string]ImplementationSettings{"spec": settings.Spec, "implementation": settings.Implementation, "verify": settings.Verify} {
			if stage.Harness != "" || stage.Model != "" || stage.ModelPolicy != "" || stage.Effort != "" {
				t.Fatalf("%s executor detail restored: %+v", name, stage)
			}
		}
		if settings.Review.FallbackHarness != "" || settings.Review.FallbackModel != "" {
			t.Fatalf("review executor detail restored: %+v", settings.Review)
		}
		if settings.Spec.TimeoutText != "25m" || settings.Implementation.TimeoutText != "5h" || settings.Review.TimeoutText != "45m" || settings.Verify.TimeoutText != "50m" {
			t.Fatalf("policy timeouts missing from composed settings: %+v", settings)
		}
		frozen := cfg.FreezePolicy()
		rawFrozen := raw.FreezePolicy()
		if !reflect.DeepEqual(frozen, rawFrozen) {
			t.Fatalf("composition changed frozen policy: %+v != %+v", frozen, rawFrozen)
		}
		policy, err := MarshalPolicyDocument(cfg)
		if err != nil {
			t.Fatal(err)
		}
		rawPolicy, err := MarshalPolicyDocument(raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(policy) != string(rawPolicy) {
			t.Fatalf("policy projection changed by composition\nraw:\n%s\nruntime:\n%s", rawPolicy, policy)
		}
		for _, leaked := range []string{"deploy-planner", "deploy-triage", "deploy-alternate", "execution_settings", "planning_models"} {
			if strings.Contains(string(policy), leaked) {
				t.Fatalf("policy projection of the runtime value carries %q:\n%s", leaked, policy)
			}
		}
	})
	t.Run("legacy document", func(t *testing.T) {
		deployment := runtimeDeployment(t)
		cfg, err := ParseRuntimeWorkspaceDocument(runtimeLegacyDocument(t), deployment, "runtime legacy")
		if err != nil {
			t.Fatal(err)
		}
		assertDeploymentControlPlane(t, cfg)
		if cfg.Workspace != "demo" || cfg.MaxBounces != 2 {
			t.Fatalf("legacy workspace policy lost: %q %d", cfg.Workspace, cfg.MaxBounces)
		}
	})
	t.Run("deployment without control-plane settings", func(t *testing.T) {
		deployment := runtimeDeployment(t)
		deployment.ExecutionSettings = nil
		deployment.PlanningModels = nil
		for name, data := range map[string][]byte{"policy-only": runtimePolicyDocument(t, 6, "25m"), "legacy": runtimeLegacyDocument(t)} {
			cfg, err := ParseRuntimeWorkspaceDocument(data, deployment, "runtime "+name)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PlanningModels != nil {
				t.Fatalf("%s: allowlist reconstructed from workspace data: %v", name, cfg.PlanningModels)
			}
			if cfg.ExecutionSettings != nil && cfg.ExecutionSettings.ControlPlane != (ControlPlaneSettings{}) {
				t.Fatalf("%s: control plane reconstructed from workspace data: %+v", name, cfg.ExecutionSettings.ControlPlane)
			}
		}
	})
	t.Run("nil deployment", func(t *testing.T) {
		if _, err := ParseRuntimeWorkspaceDocument(runtimePolicyDocument(t, 6, "25m"), nil, "runtime nil"); err == nil || !strings.Contains(err.Error(), "deployment configuration is required") {
			t.Fatalf("nil deployment error = %v", err)
		}
	})
	t.Run("parse errors propagate", func(t *testing.T) {
		if _, err := ParseRuntimeWorkspaceDocument([]byte("workspace: other\n"), runtimeDeployment(t), "runtime foreign"); err == nil || !strings.Contains(err.Error(), "workspace must remain") {
			t.Fatalf("foreign workspace error = %v", err)
		}
	})
}

func TestRuntimeWorkspaceConfigIsolatesConcurrentResults(t *testing.T) {
	deployment := runtimeDeployment(t)
	for stage, route := range deployment.Routing.Stages {
		route.LegacyHarnesses = []string{"legacy-" + stage}
		deployment.Routing.Stages[stage] = route
	}
	beforeValue := fmt.Sprintf("%#v %#v", deployment, *deployment.ExecutionSettings)
	before, err := yaml.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}

	first, err := ParseRuntimeWorkspaceDocument(runtimePolicyDocument(t, 3, "21m"), deployment, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseRuntimeWorkspaceDocument(runtimePolicyDocument(t, 4, "22m"), deployment, "second")
	if err != nil {
		t.Fatal(err)
	}
	first.ExecutionSettings.ControlPlane.Planning.Model = "mutated"
	first.ExecutionSettings.ControlPlane.Triage.Model = "mutated"
	first.PlanningModels[0] = "mutated"
	first.PlanningModels = append(first.PlanningModels, "appended")
	triage := first.Routing.Stages["triage"]
	triage.Model = "mutated"
	triage.LegacyHarnesses[0] = "mutated"
	first.Routing.Stages["triage"] = triage
	first.Routing.Stages["spec"] = StageRoute{TimeoutText: "1m"}
	assertDeploymentControlPlane(t, second)
	if second.Routing.Stages["spec"].TimeoutText != "22m" || second.MaxBounces != 4 {
		t.Fatalf("second result shares state with the first: %+v", second.Routing.Stages["spec"])
	}
	if got := second.Routing.Stages["triage"].LegacyHarnesses; !slices.Equal(got, []string{"legacy-triage"}) {
		t.Fatalf("second triage route shares legacy harnesses: %v", got)
	}
	after, err := yaml.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || beforeValue != fmt.Sprintf("%#v %#v", deployment, *deployment.ExecutionSettings) {
		t.Fatalf("deployment mutated through a runtime result\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Concurrent compositions for distinct workspace policies run under the
	// race detector in make test; each goroutine mutates only its own result.
	// A closed channel releases every goroutine at once so the compositions
	// overlap deterministically, without sleeps.
	const goroutines = 16
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wait sync.WaitGroup
	for i := range goroutines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			bounces := 2 + i
			spec := fmt.Sprintf("%dm", 20+i)
			cfg, parseErr := ParseRuntimeWorkspaceDocument(runtimePolicyDocument(t, bounces, spec), deployment, "concurrent")
			if parseErr != nil {
				errs <- parseErr
				return
			}
			if cfg.MaxBounces != bounces || cfg.Routing.Stages["spec"].TimeoutText != spec ||
				cfg.ExecutionSettings.ControlPlane.Planning.Model != "deploy-planner" || cfg.Routing.Stages["triage"].Model != "deploy-triage" ||
				!slices.Equal(cfg.PlanningModels, []string{"deploy-planner", "deploy-alternate"}) {
				errs <- fmt.Errorf("goroutine %d composed bounces=%d spec=%q planning=%q allowlist=%v", i, cfg.MaxBounces, cfg.Routing.Stages["spec"].TimeoutText, cfg.ExecutionSettings.ControlPlane.Planning.Model, cfg.PlanningModels)
				return
			}
			cfg.ExecutionSettings.ControlPlane.Planning.Model = fmt.Sprintf("mutated-%d", i)
			cfg.PlanningModels[0] = fmt.Sprintf("mutated-%d", i)
			route := cfg.Routing.Stages["triage"]
			route.LegacyHarnesses[0] = fmt.Sprintf("mutated-%d", i)
			cfg.Routing.Stages["triage"] = route
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	after, err = yaml.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("concurrent compositions mutated the deployment\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
