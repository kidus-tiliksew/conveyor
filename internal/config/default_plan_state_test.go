package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNormalizeDefaultPlanState covers component-runtime invariant 39: a
// stated value is not_required or needed, the legacy spec_approval is read
// only when default_plan_state is absent, a disagreeing pair is refused
// naming the field, and absent both it is needed.
func TestNormalizeDefaultPlanState(t *testing.T) {
	on, off := true, false
	for _, test := range []struct {
		name     string
		in       ExecutionPolicy
		want     string
		refusing string
	}{
		{"absent both", ExecutionPolicy{}, "needed", ""},
		{"absent both with other keys", ExecutionPolicy{MergeApproval: true, ImplementConcurrency: 2}, "needed", ""},
		{"stated needed", ExecutionPolicy{DefaultPlanState: "needed"}, "needed", ""},
		{"stated not_required", ExecutionPolicy{DefaultPlanState: "not_required"}, "not_required", ""},
		{"alias true", ExecutionPolicy{SpecApproval: &on}, "needed", ""},
		{"alias false", ExecutionPolicy{SpecApproval: &off}, "not_required", ""},
		{"agreeing pair", ExecutionPolicy{DefaultPlanState: "not_required", SpecApproval: &off}, "not_required", ""},
		{"disagreeing pair", ExecutionPolicy{DefaultPlanState: "needed", SpecApproval: &off}, "", "execution.default_plan_state"},
		{"disagreeing pair the other way", ExecutionPolicy{DefaultPlanState: "not_required", SpecApproval: &on}, "", "execution.default_plan_state"},
		{"invalid value", ExecutionPolicy{DefaultPlanState: "completed"}, "", "execution.default_plan_state"},
	} {
		execution := test.in
		err := NormalizeDefaultPlanState(&execution)
		if test.refusing != "" {
			if err == nil || !strings.Contains(err.Error(), test.refusing) {
				t.Fatalf("%s: err=%v, want a refusal naming %s", test.name, err, test.refusing)
			}
			continue
		}
		if err != nil || execution.DefaultPlanState != test.want || execution.SpecApproval != nil {
			t.Fatalf("%s: execution=%+v err=%v, want %s and no alias", test.name, execution, err, test.want)
		}
	}
}

// TestDefaultPlanStateOnEveryLoadPath proves the deployment file, a workspace
// document (the path of PUT, import, and creation), and a stored row read the
// alias, refuse a bad value or a disagreeing pair, default to needed with an
// absent or empty block, and keep the merge gate's default unchanged
// (component-runtime invariant 39; DEC-125(1)).
func TestDefaultPlanStateOnEveryLoadPath(t *testing.T) {
	_ = captureRetiredExecutionKeyWarnings(t)
	t.Setenv("HOME", t.TempDir())
	example, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := normalizeDeployment(validConfig(), "plan state deployment")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := MarshalPolicyDocument(deployment)
	if err != nil {
		t.Fatal(err)
	}
	loaders := executionBlockLoaders(t, example, deployment, policy)
	for _, test := range []struct {
		name  string
		block string
		want  string
	}{
		{"empty block", "{}", "needed"},
		{"block without either key", "{merge_approval: true, implement_concurrency: 2}", "needed"},
		{"stated not_required", "{default_plan_state: not_required, merge_approval: true}", "not_required"},
		{"stated needed", "{default_plan_state: needed}", "needed"},
		{"alias false", "{spec_approval: false, merge_approval: true}", "not_required"},
		{"alias true", "{spec_approval: true, merge_approval: false}", "needed"},
		{"agreeing pair", "{default_plan_state: needed, spec_approval: true}", "needed"},
	} {
		for loaderName, load := range loaders {
			got, loadErr := load(test.block)
			if loadErr != nil {
				t.Fatalf("%s %s: %v", loaderName, test.name, loadErr)
			}
			if got.DefaultPlanState != test.want || got.SpecApproval != nil {
				t.Fatalf("%s %s: execution=%+v, want %s and no alias", loaderName, test.name, got, test.want)
			}
		}
	}
	for _, block := range []string{
		"{default_plan_state: needed, spec_approval: false}",
		"{default_plan_state: not_required, spec_approval: true}",
		"{default_plan_state: completed}",
	} {
		for loaderName, load := range loaders {
			if _, loadErr := load(block); loadErr == nil || !strings.Contains(loadErr.Error(), "execution.default_plan_state") {
				t.Fatalf("%s %s: err=%v, want a refusal naming execution.default_plan_state", loaderName, block, loadErr)
			}
		}
	}

	// An absent execution block in the deployment file reads needed and keeps
	// the merge gate on; the plan-state keys never change the merge default.
	for _, test := range []struct {
		name  string
		block string
		merge bool
	}{
		{"absent block", "", true},
		{"empty block", "{}", true},
		{"only default_plan_state", "{default_plan_state: not_required}", true},
		{"only alias false", "{spec_approval: false}", true},
		{"only alias true", "{spec_approval: true}", false},
	} {
		data := example
		var root yaml.Node
		if err = yaml.Unmarshal(example, &root); err != nil {
			t.Fatal(err)
		}
		removeDirectMappingKey(root.Content[0], "execution")
		if data, err = yaml.Marshal(&root); err != nil {
			t.Fatal(err)
		}
		if test.block != "" {
			data = withExecutionBlock(t, data, test.block)
		}
		path := filepath.Join(t.TempDir(), "conveyor.yaml")
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, loadErr := loadDeploymentForTest(path)
		if loadErr != nil {
			t.Fatalf("%s: %v", test.name, loadErr)
		}
		if cfg.Execution.MergeApproval != test.merge {
			t.Fatalf("%s: merge_approval=%t, want %t", test.name, cfg.Execution.MergeApproval, test.merge)
		}
		if test.block == "" && cfg.Execution.DefaultPlanState != "needed" {
			t.Fatalf("%s: default_plan_state=%q, want needed", test.name, cfg.Execution.DefaultPlanState)
		}
	}
}

// TestStoredRowsAndExportServeOnlyDefaultPlanState proves a stored row
// carrying only the legacy alias reads as default_plan_state, and that the
// stored document, the workspace read, and config export carry only
// default_plan_state (component-runtime invariant 39).
func TestStoredRowsAndExportServeOnlyDefaultPlanState(t *testing.T) {
	_ = captureRetiredExecutionKeyWarnings(t)
	t.Setenv("HOME", t.TempDir())
	deployment, err := normalizeDeployment(validConfig(), "plan state export")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := MarshalPolicyDocument(deployment)
	if err != nil {
		t.Fatal(err)
	}
	stored := withExecutionBlock(t, policy, "{spec_approval: false, merge_approval: true}")
	decoded, err := DecodeStoredWorkspaceDocument(stored)
	if err != nil || decoded.Execution.DefaultPlanState != "not_required" || decoded.Execution.SpecApproval != nil {
		t.Fatalf("decoded stored row=%+v err=%v", decoded.Execution, err)
	}
	cfg, _, err := ParseStoredWorkspaceDocument(stored, deployment, "plan state stored row")
	if err != nil || cfg.Execution.DefaultPlanState != "not_required" {
		t.Fatalf("parsed stored row=%+v err=%v", cfg, err)
	}
	written, err := MarshalPolicyDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "spec_approval") || !strings.Contains(string(written), "default_plan_state: not_required") {
		t.Fatalf("stored write=%s", written)
	}
	served, err := json.Marshal(cfg.PolicyDocument())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(served), "spec_approval") || !strings.Contains(string(served), `"default_plan_state":"not_required"`) {
		t.Fatalf("served document=%s", served)
	}
	exported, err := yaml.Marshal(cfg.PolicyDocument())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), "spec_approval") || !strings.Contains(string(exported), "default_plan_state: not_required") {
		t.Fatalf("exported document=%s", exported)
	}
}

// mergeDefaultCase is one execution block and the merge_approval each load
// path yields for it. The legacy rows hold the values the release before the
// plan state yields for the same block on the same path, measured from that
// release's source; each canonical row holds the values of the legacy block it
// means (default_plan_state needed as spec_approval true, not_required as
// spec_approval false).
type mergeDefaultCase struct {
	block, legacyEquivalent               string
	file, workspace, stored, storedLegacy bool
}

var mergeDefaultCases = []mergeDefaultCase{
	{"<absent>", "", true, false, false, true},
	{"{}", "", true, false, false, true},
	{"{merge_approval: false}", "", true, false, false, true},
	{"{merge_approval: true}", "", true, true, true, true},
	{"{implement_concurrency: 2}", "", false, false, false, false},
	{"{spec_approval: true}", "", false, false, false, false},
	{"{spec_approval: false}", "", true, false, false, true},
	{"{spec_approval: true, merge_approval: false}", "", false, false, false, false},
	{"{spec_approval: true, merge_approval: true}", "", true, true, true, true},
	{"{spec_approval: false, merge_approval: false}", "", true, false, false, true},
	{"{spec_approval: false, merge_approval: true}", "", true, true, true, true},
	{"{default_plan_state: needed}", "{spec_approval: true}", false, false, false, false},
	{"{default_plan_state: not_required}", "{spec_approval: false}", true, false, false, true},
	{"{default_plan_state: needed, merge_approval: false}", "{spec_approval: true, merge_approval: false}", false, false, false, false},
	{"{default_plan_state: needed, merge_approval: true}", "{spec_approval: true, merge_approval: true}", true, true, true, true},
	{"{default_plan_state: not_required, merge_approval: false}", "{spec_approval: false, merge_approval: false}", true, false, false, true},
	{"{default_plan_state: not_required, merge_approval: true}", "{spec_approval: false, merge_approval: true}", true, true, true, true},
	{"{default_plan_state: needed, spec_approval: true, merge_approval: false}", "{spec_approval: true, merge_approval: false}", false, false, false, false},
}

// TestMergeDefaultIsUnchangedByThePlanStateKey proves the merge gate's default
// is the one the release before the plan state computed, on the deployment
// file, its export and reload, a workspace document (the path of PUT, import,
// and workspace creation), a stored row, and a stored row of the legacy shape,
// and that each plan-state spelling yields what its legacy equivalent yields
// (DEC-125(1); component-task-lifecycle invariant 18;
// req-review-gates-evidence REQ-1).
func TestMergeDefaultIsUnchangedByThePlanStateKey(t *testing.T) {
	_ = captureRetiredExecutionKeyWarnings(t)
	t.Setenv("HOME", t.TempDir())
	example, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	setBlock := func(data []byte, block string) []byte {
		t.Helper()
		var root yaml.Node
		if err := yaml.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		removeDirectMappingKey(root.Content[0], "execution")
		out, err := yaml.Marshal(&root)
		if err != nil {
			t.Fatal(err)
		}
		if block == "<absent>" {
			return out
		}
		return withExecutionBlock(t, out, block)
	}
	writeFile := func(data []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "conveyor.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	base, err := loadDeploymentForTest(writeFile(example))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := MarshalPolicyDocument(base)
	if err != nil {
		t.Fatal(err)
	}
	legacyShape := append([]byte("execution_settings: {implementation: {timeout: 4h}}\n"), policy...)
	type outcome struct{ file, reload, workspace, stored, storedLegacy bool }
	resolve := func(block string) outcome {
		t.Helper()
		var got outcome
		cfg, err := loadDeploymentForTest(writeFile(setBlock(example, block)))
		if err != nil {
			t.Fatalf("%s: file load: %v", block, err)
		}
		got.file = cfg.Execution.MergeApproval
		exported, err := MarshalDeployment(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(exported), "spec_approval") {
			t.Fatalf("%s: export carries spec_approval:\n%s", block, exported)
		}
		reloaded, err := loadDeploymentForTest(writeFile(exported))
		if err != nil {
			t.Fatalf("%s: reload: %v", block, err)
		}
		got.reload = reloaded.Execution.MergeApproval
		if reloaded.Execution.DefaultPlanState != cfg.Execution.DefaultPlanState {
			t.Fatalf("%s: reload default_plan_state=%q, want %q", block, reloaded.Execution.DefaultPlanState, cfg.Execution.DefaultPlanState)
		}
		workspace, err := ParseWorkspaceDocument(setBlock(policy, block), base, "merge default workspace")
		if err != nil {
			t.Fatalf("%s: workspace document: %v", block, err)
		}
		got.workspace = workspace.Execution.MergeApproval
		stored, _, err := ParseStoredWorkspaceDocument(setBlock(policy, block), base, "merge default stored")
		if err != nil {
			t.Fatalf("%s: stored row: %v", block, err)
		}
		got.stored = stored.Execution.MergeApproval
		storedLegacy, _, err := ParseStoredWorkspaceDocument(setBlock(legacyShape, block), base, "merge default legacy stored")
		if err != nil {
			t.Fatalf("%s: legacy stored row: %v", block, err)
		}
		got.storedLegacy = storedLegacy.Execution.MergeApproval
		return got
	}
	for _, test := range mergeDefaultCases {
		got := resolve(test.block)
		want := outcome{test.file, test.file, test.workspace, test.stored, test.storedLegacy}
		if got != want {
			t.Fatalf("%s: merge_approval %+v, want %+v", test.block, got, want)
		}
		if test.legacyEquivalent != "" {
			if legacy := resolve(test.legacyEquivalent); legacy != got {
				t.Fatalf("%s resolves merge_approval %+v, but its legacy equivalent %s resolves %+v", test.block, got, test.legacyEquivalent, legacy)
			}
		}
	}
}
