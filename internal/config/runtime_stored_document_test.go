package config

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
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
// setup declared (DEC-104(1), DEC-104(2)).
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
			if got := cfg.Routing.Stages[stage]; got.TimeoutText != timeout || got.Execution != ExecutionMCP {
				t.Fatalf("%s %s route = %+v, want only timeout %s", name, stage, got, timeout)
			}
		}
		if len(cfg.Review.Seats) != 3 || cfg.MaxBounces != 5 || cfg.Execution.DefaultPlanState != "not_required" || !cfg.Execution.MergeApproval {
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
		runtime.Routing.Triage.Model != "deploy-triage" || !reflect.DeepEqual(runtime.PlanningModels, []string{"deploy-planner", "deploy-alternate"}) {
		t.Fatalf("runtime composition lost the deployment control plane: %+v triage=%+v", runtime.ExecutionSettings, runtime.Routing.Triage)
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
	if stored.Execution.DefaultPlanState != "needed" || !stored.Execution.MergeApproval {
		t.Fatalf("legacy row without an execution block lost the gate defaults: %+v", stored.Execution)
	}
	if _, err := DecodeStoredWorkspaceDocument([]byte(strings.Replace(row, "workspace: demo", "workspace: demo\nunknown_key: true", 1))); err == nil {
		t.Fatal("an unrelated unknown key decoded")
	}
}

// storedRowLog replaces the stored-row logging seam for one test and returns
// the lines it records. Tests that use it don't run in parallel.
func storedRowLog(t *testing.T) *warningLog {
	t.Helper()
	var lines warningLog
	storedExecutionDetailLog.Lock()
	previousLogf, previousLogged := storedExecutionDetailLog.logf, storedExecutionDetailLog.logged
	storedExecutionDetailLog.logf, storedExecutionDetailLog.logged = lines.logf, map[string]bool{}
	storedExecutionDetailLog.Unlock()
	t.Cleanup(func() {
		storedExecutionDetailLog.Lock()
		storedExecutionDetailLog.logf, storedExecutionDetailLog.logged = previousLogf, previousLogged
		storedExecutionDetailLog.Unlock()
	})
	return &lines
}

// everyRemovedKeyWorkspaceRow is a stored row that carries every agent-stage
// key this release removed from the configuration types, most with values
// the earlier validation would refuse, beside its policy: route model,
// model policy, harness, effort, harnesses, and model tier, the triage
// route's model, model tier, and effort, the stage settings' harness, model,
// model policy, and effort, the review fallbacks, and seat model, harness,
// and effort (component-runtime invariants 13 and 15).
const everyRemovedKeyWorkspaceRow = `workspace: demo
max_bounces: 4
execution_settings:
  control_plane:
    spec: {model: "stored spec!", effort: extreme, timeout: 31m}
  spec: {harness: "bad harness!", model: "", model_policy: bogus, effort: extreme, timeout: 32m}
  implementation: {harness: missing, model: stored-impl, model_policy: explicit, effort: maximal, timeout: 3h}
  verify: {harness: missing, model: stored-verify, model_policy: harness_default, effort: high, timeout: 75m}
  review: {execution: sideways, timeout: 55m, fallback_model: stored-fallback, fallback_harness: missing}
routing:
  stages:
    triage: {model: stored-triage, model_tier: stored-tier, effort: extreme, timeout: 9m, execution: mcp}
    implement: {model: stored-route, model_policy: nonsense, harness: missing, effort: extreme, harnesses: [a, b], model_tier: big, timeout: 7h, execution: in_process}
    review: {model: stored-review, harness: missing, effort: high, timeout: 65m, execution: in_process}
review:
  distinct_models: true
  seats:
    - {model: stored-seat-a, harness: missing, effort: extreme}
    - {model: ""}
    - {}
repos:
  - {name: conveyor, url: "https://github.com/example/conveyor", base: main}
`

// Every removed key in a stored row loads on every read path, is logged once
// by name and never by value, and leaves the timeouts, seat count,
// distinct_models, and gate defaults the row resolved to; the triage route
// comes only from the deployment, and the row is not changed
// (component-runtime invariants 13 to 15).
func TestStoredRowWithEveryRemovedKeyLoadsAndLogsNames(t *testing.T) {
	lines := storedRowLog(t)
	deployment := runtimeDeployment(t)
	row := []byte(everyRemovedKeyWorkspaceRow)
	original := append([]byte(nil), row...)

	document, err := DecodeStoredWorkspaceDocument(row)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(document.Review.Seats) != 3 || !document.Review.DistinctModels || document.MaxBounces != 4 {
		t.Fatalf("decoded policy = %+v", document)
	}
	stored, legacy, err := ParseStoredWorkspaceDocument(row, deployment, "stored row")
	if err != nil || !legacy {
		t.Fatalf("parse: legacy=%t err=%v", legacy, err)
	}
	runtime, err := ParseRuntimeWorkspaceDocument(row, deployment, "stored row")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	for name, cfg := range map[string]*Config{"stored": stored, "runtime": runtime} {
		for stage, timeout := range map[string]string{"spec": "32m", "implement": "3h", "review": "55m", "verify": "75m"} {
			if got := cfg.Routing.Stages[stage]; got.TimeoutText != timeout || got.Execution != ExecutionMCP {
				t.Fatalf("%s %s route = %+v, want timeout %s over MCP", name, stage, got, timeout)
			}
		}
		if len(cfg.Review.Seats) != 3 || !cfg.Review.DistinctModels || cfg.MaxBounces != 4 ||
			cfg.Execution.DefaultPlanState != DefaultPlanStateNeeded || !cfg.Execution.MergeApproval {
			t.Fatalf("%s policy = seats %d distinct %t bounces %d execution %+v", name, len(cfg.Review.Seats), cfg.Review.DistinctModels, cfg.MaxBounces, cfg.Execution)
		}
		if _, ok := cfg.Routing.Stages["triage"]; ok {
			t.Fatalf("%s took a triage stage route from the row: %+v", name, cfg.Routing.Stages)
		}
		if cfg.Routing.Triage != deployment.Routing.Triage {
			t.Fatalf("%s triage route = %+v, want the deployment's %+v", name, cfg.Routing.Triage, deployment.Routing.Triage)
		}
	}
	if runtime.ExecutionSettings.ControlPlane.Triage.Model != "deploy-triage" {
		t.Fatalf("runtime control plane = %+v", runtime.ExecutionSettings.ControlPlane)
	}
	if !bytes.Equal(row, original) {
		t.Fatal("reading changed the stored row")
	}
	got := lines.all()
	if len(got) != 1 {
		t.Fatalf("stored-row log = %q, want one line for repeated reads of one row", got)
	}
	for _, name := range []string{
		"execution_settings.control_plane.spec.model", "execution_settings.control_plane.spec.effort",
		"execution_settings.spec.harness", "execution_settings.spec.model", "execution_settings.spec.model_policy", "execution_settings.spec.effort",
		"execution_settings.implementation.harness", "execution_settings.verify.model",
		"execution_settings.review.execution", "execution_settings.review.fallback_model", "execution_settings.review.fallback_harness",
		"routing.stages.triage.model", "routing.stages.triage.model_tier", "routing.stages.triage.effort", "routing.stages.triage.execution",
		"routing.stages.implement.model", "routing.stages.implement.model_policy", "routing.stages.implement.harness",
		"routing.stages.implement.effort", "routing.stages.implement.harnesses", "routing.stages.implement.model_tier",
		"routing.stages.review.model", "review.seats[].model", "review.seats[].harness", "review.seats[].effort",
	} {
		if !strings.Contains(got[0], name) {
			t.Fatalf("stored-row log %q does not name %s", got[0], name)
		}
	}
	for _, value := range []string{"stored spec!", "extreme", "bad harness!", "bogus", "stored-impl", "maximal", "stored-verify", "sideways", "stored-fallback",
		"stored-triage", "stored-tier", "stored-route", "nonsense", "missing", "stored-review", "stored-seat-a"} {
		if strings.Contains(got[0], value) {
			t.Fatalf("stored-row log %q echoes value %q", got[0], value)
		}
	}
}

// The stored-row log is safe under concurrent reads and still names each
// distinct set of removed keys once (component-runtime invariant 14).
func TestStoredRowLogIsConcurrencySafe(t *testing.T) {
	lines := storedRowLog(t)
	deployment := runtimeDeployment(t)
	rows := [][]byte{
		[]byte(everyRemovedKeyWorkspaceRow),
		[]byte("workspace: demo\nreview: {seats: [{model: only-seat}]}\nrepos: []\n"),
	}
	var wait sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func(row []byte) {
			defer wait.Done()
			if _, err := ParseRuntimeWorkspaceDocument(row, deployment, "concurrent row"); err != nil {
				errs <- err
			}
		}(rows[i%len(rows)])
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got := lines.all()
	if len(got) != 2 {
		t.Fatalf("stored-row log = %q, want one line per distinct row shape", got)
	}
	for _, line := range got {
		if strings.Contains(line, "only-seat") || strings.Contains(line, "stored-") {
			t.Fatalf("stored-row log %q echoes a value", line)
		}
	}
}

// A row without removed keys reads without a log line.
func TestCanonicalStoredRowLogsNothing(t *testing.T) {
	lines := storedRowLog(t)
	if _, err := DecodeStoredWorkspaceDocument([]byte("workspace: demo\nstage_timeouts: {implement: 2h}\nreview: {seats: [{}]}\nrepos: []\n")); err != nil {
		t.Fatal(err)
	}
	if got := lines.all(); len(got) != 0 {
		t.Fatalf("canonical row logged %q", got)
	}
}
