package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A conveyor.yaml that an earlier CLI wrote still loads: every execution key
// is removed before validation and named once in one warning line, and the
// client-local settings survive (DEC-96(2); component-runtime).
func TestLoadClientIgnoresExecutionKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	document := `# earlier client file
workspace: demo
harnesses:
  - &codex
    name: codex
    command: ["not", "{a", "valid", "harness"]
setups:
  - name: default
    execution_settings: {implementation: {harness: codex, model: secret-model-value}}
default_setup: default
execution_settings:
  implementation: {harness: missing, effort: impossible}
routing:
  stages: {implement: {harness: missing}}
review:
  seats: [{harness: missing, model: m}]
execution:
  verify_concurrency: -4
  first_activity_timeout: not-a-duration
planning_models: [secret-model-value]
kit_permissions:
  - {server: "https://conveyor.example", workspace: demo, repository: conveyor, binding: fixture, actions: []}
worktree_location:
  mode: machine
  root: ~/trees
`
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	loaded, err := LoadClient(path, logf)
	if err != nil {
		t.Fatal(err)
	}
	want := "config: " + path + ": ignoring execution keys default_setup, execution, execution_settings, harnesses, planning_models, review, routing, setups; Conveyor keeps no local execution setup (DEC-96)"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("warnings = %q, want one line %q", lines, want)
	}
	if strings.Contains(lines[0], "secret-model-value") || strings.Contains(lines[0], "impossible") {
		t.Fatalf("warning names a value: %q", lines[0])
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location.Root != filepath.Join(home, "trees") {
		t.Fatalf("worktree location = %+v, %v", location, ok)
	}
	if len(loaded.KitPermissions) != 1 || loaded.KitPermissions[0].Binding != "fixture" {
		t.Fatalf("kit permissions = %+v", loaded.KitPermissions)
	}
	if loaded.ExecutionSettings != nil || len(loaded.Routing.Stages) != 0 || len(loaded.PlanningModels) != 0 {
		t.Fatalf("execution detail survived: %+v", loaded)
	}

	clean := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(clean, []byte("worktree_location:\n  mode: repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines = nil
	if _, err := LoadClient(clean, logf); err != nil || len(lines) != 0 {
		t.Fatalf("clean file: err=%v warnings=%q", err, lines)
	}

	unknown := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(unknown, []byte("harnesses: []\nnot_a_key: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClient(unknown, nil); err == nil || !strings.Contains(err.Error(), "not_a_key") {
		t.Fatalf("unknown key error = %v", err)
	}
	both := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(both, []byte("worktree_root: /srv/a\nworktree_location: {mode: machine}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClient(both, nil); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both location keys error = %v", err)
	}
	empty := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadClient(empty, nil); err != nil || loaded == nil {
		t.Fatalf("empty file: %v", err)
	}
}
