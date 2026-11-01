package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// legacyExecutorWorkspaceRow is a stored workspace document an earlier
// release wrote: a harness registry whose argv would fail the retired
// validation, two setups with executor detail, a default setup, a stale
// top-level projection and routing, and review seat contents.
const legacyExecutorWorkspaceRow = `workspace: demo
max_bounces: 5
work_order_queue_timeout: 12h
harnesses:
  - name: codex
    mcp_transport: toml_override
    command: [codex, exec]
    probe_command: [codex, --version]
    probe_timeout: 10s
setups:
  - name: other
    execution_settings:
      spec: {harness: codex, model: other-spec, timeout: 11m}
      implementation: {harness: codex, model: other-impl, timeout: 12m}
      review: {execution: mcp, timeout: 13m}
    review: {seats: [{model: other-seat}]}
  - name: fast
    refresh_review: full
    execution_settings:
      control_plane:
        triage: {model: setup-triage, timeout: 25m}
      spec: {harness: codex, model: setup-spec, model_policy: explicit, effort: high, timeout: 45m}
      implementation: {harness: codex, model_policy: bogus, effort: high, timeout: 5h}
      verify: {harness: codex, model: setup-verify, timeout: 90m}
      review: {execution: mcp, timeout: 2h, fallback_harness: codex, fallback_model: setup-fallback}
    review:
      seats:
        - {model: seat-a, harness: codex}
        - {model: seat-b}
        - {model: seat-c, effort: high}
default_setup: fast
execution_settings:
  spec: {harness: codex, model: stale-spec, timeout: 10m}
  implementation: {harness: codex, model: stale-impl, timeout: 1h}
  review: {execution: mcp, timeout: 20m}
routing:
  stages:
    implement: {model: stale-routing, harness: codex, timeout: 9m, execution: mcp}
review: {seats: [{model: stale-seat}]}
execution: {spec_approval: false, merge_approval: true, implement_concurrency: 2, review_concurrency: 1}
repos:
  - {name: conveyor, url: "https://github.com/example/conveyor", base: main}
`

// component-runtime "Configuration scopes": a stored row carrying legacy
// harness, setup, default-setup, and executor keys loads on every read path
// without validation or mutation, and yields the policy its named default
// setup declared (DEC-96(1), DEC-96(2)).
func TestStoredWorkspaceDocumentIgnoresLegacyExecutionKeys(t *testing.T) {
	deployment := runtimeDeployment(t)
	row := []byte(legacyExecutorWorkspaceRow)
	original := append([]byte(nil), row...)

	document, err := DecodeStoredWorkspaceDocument(row)
	if err != nil {
		t.Fatalf("decode legacy row: %v", err)
	}
	if document.MaxBounces != 5 || len(document.Repos) != 1 || document.Execution.ImplementConcurrency != 2 {
		t.Fatalf("decoded row lost policy: %+v", document)
	}
	if got := len(document.Review.Seats); got != 3 {
		t.Fatalf("decoded row took %d review seats, want the default setup's 3", got)
	}
	for i, seat := range document.Review.Seats {
		if seat != (ReviewSeat{}) {
			t.Fatalf("decoded row kept review seat %d contents: %+v", i, seat)
		}
	}
	if document.ExecutionSettings == nil || document.ExecutionSettings.Implementation != (ImplementationSettings{TimeoutText: "5h"}) {
		t.Fatalf("decoded row kept executor detail or lost the default setup: %+v", document.ExecutionSettings)
	}

	stored, legacy, err := ParseStoredWorkspaceDocument(row, deployment, "legacy row")
	if err != nil {
		t.Fatalf("parse legacy row: %v", err)
	}
	if !legacy {
		t.Fatal("legacy row was not reported for canonicalization")
	}
	runtime, err := ParseRuntimeWorkspaceDocument(row, deployment, "legacy row")
	if err != nil {
		t.Fatalf("compose legacy row: %v", err)
	}
	want := map[string]string{"spec": "45m", "implement": "5h", "review": "2h", "verify": "90m"}
	for name, cfg := range map[string]*Config{"stored": stored, "runtime": runtime} {
		for stage, timeout := range want {
			if got := cfg.Routing.Stages[stage]; got.TimeoutText != timeout || got.Model != "" || got.Harness != "" || got.Effort != "" || got.Execution != ExecutionMCP {
				t.Fatalf("%s %s route = %+v, want only timeout %s", name, stage, got, timeout)
			}
		}
		if len(cfg.Review.Seats) != 3 || cfg.MaxBounces != 5 || cfg.Execution.SpecApproval || !cfg.Execution.MergeApproval {
			t.Fatalf("%s policy = seats %d bounces %d execution %+v", name, len(cfg.Review.Seats), cfg.MaxBounces, cfg.Execution)
		}
		frozen := cfg.FreezePolicy()
		if frozen.StageTimeouts != (PolicyStageTimeouts{Spec: "45m", Implement: "5h", Review: "2h", Verify: "90m"}) || len(frozen.Review.Seats) != 3 {
			t.Fatalf("%s froze %+v", name, frozen)
		}
	}
	if stored.ExecutionSettings != nil || stored.PlanningModels != nil {
		t.Fatalf("stored parse kept execution settings: %+v %v", stored.ExecutionSettings, stored.PlanningModels)
	}
	if runtime.ExecutionSettings == nil || runtime.ExecutionSettings.ControlPlane.Triage.Model != "deploy-triage" ||
		runtime.Routing.Stages["triage"].Model != "deploy-triage" || !reflect.DeepEqual(runtime.PlanningModels, []string{"deploy-planner", "deploy-alternate"}) {
		t.Fatalf("runtime composition lost the deployment control plane: %+v triage=%+v", runtime.ExecutionSettings, runtime.Routing.Stages["triage"])
	}
	for _, settings := range []ImplementationSettings{runtime.ExecutionSettings.Spec, runtime.ExecutionSettings.Implementation, runtime.ExecutionSettings.Verify} {
		if settings.Harness != "" || settings.Model != "" || settings.Effort != "" || settings.ModelPolicy != "" {
			t.Fatalf("runtime composition kept executor settings: %+v", runtime.ExecutionSettings)
		}
	}
	policy, err := MarshalPolicyDocument(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"harness", "setups", "default_setup", "codex", "setup-spec", "seat-a"} {
		if bytes.Contains(policy, []byte(retired)) {
			t.Fatalf("canonical policy document carries %q:\n%s", retired, policy)
		}
	}
	if !bytes.Equal(row, original) {
		t.Fatal("parsing changed the stored row")
	}
}

// A legacy row without a default setup takes its stage timeouts from the
// top-level execution settings, then from its routes, and a legacy row
// without an execution block keeps both gates on.
func TestStoredWorkspaceDocumentWithoutDefaultSetupUsesTopLevelTimeouts(t *testing.T) {
	row := `workspace: demo
harnesses:
  - {name: codex, command: [codex]}
execution_settings:
  control_plane:
    spec: {model: legacy-spec, timeout: 33m}
  implementation: {harness: codex, model: impl, timeout: 3h}
routing:
  stages:
    review: {model: reviewer, harness: codex, timeout: 70m}
    verify: {timeout: 80m}
repos:
  - {name: conveyor, url: "https://github.com/example/conveyor", base: main}
`
	stored, _, err := ParseStoredWorkspaceDocument([]byte(row), runtimeDeployment(t), "top-level row")
	if err != nil {
		t.Fatal(err)
	}
	for stage, timeout := range map[string]string{"spec": "33m", "implement": "3h", "review": "70m", "verify": "80m"} {
		if got := stored.Routing.Stages[stage].TimeoutText; got != timeout {
			t.Fatalf("%s timeout = %q, want %q", stage, got, timeout)
		}
	}
	if !stored.Execution.SpecApproval || !stored.Execution.MergeApproval {
		t.Fatalf("legacy row without an execution block lost the gate defaults: %+v", stored.Execution)
	}
	if _, err := DecodeStoredWorkspaceDocument([]byte(strings.Replace(row, "workspace: demo", "workspace: demo\nunknown_key: true", 1))); err == nil {
		t.Fatal("an unrelated unknown key decoded")
	}
}
