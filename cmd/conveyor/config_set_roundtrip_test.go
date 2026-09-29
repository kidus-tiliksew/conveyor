package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"gopkg.in/yaml.v3"
)

// multiSetupRoundTripConfig mirrors the devbox file observed 2026-09-29: named
// setups with a non-first default, non-default planning context limits, and a
// review fallback model that differs from the first review seat. The top-level
// execution_settings/review block is the default-setup projection.
const multiSetupRoundTripConfig = `workspace: demo
max_bounces: 10
work_order_queue_timeout: 24h
execution_settings: &gpt_settings
  control_plane:
    triage: {model: gpt-5.6-luna, timeout: 20m}
    planning:
      model: gpt-5.6-luna
      timeout: 20m
      exploration_output_tokens: 12345
      context: {depth: 3, nodes: 17, renderable_bytes: 131072, artifact_refs: 9, authority_nodes: 40}
  spec: {harness: codex, model: gpt-5.6-sol, model_policy: explicit, effort: high, timeout: 30m}
  implementation: {harness: codex, model: gpt-5.6-sol, model_policy: explicit, effort: medium, timeout: 4h}
  review: {execution: mcp, timeout: 1h, fallback_model: gpt-5.6-terra, fallback_harness: codex}
review: &gpt_review
  seats:
    - {model: gpt-5.6-luna, harness: codex, effort: high}
execution:
  spec_approval: true
  merge_approval: true
  implement_concurrency: 1
  review_concurrency: 1
  first_activity_timeout: 2m
harnesses:
HARNESSES
default_setup: gpt-setup
setups:
  - name: claude-setup
    refresh_review: delta
    execution_settings:
      control_plane:
        triage: {model: gpt-5.6-luna, timeout: 20m}
        planning:
          model: gpt-5.6-luna
          timeout: 20m
          exploration_output_tokens: 23456
          context: {depth: 4, nodes: 19, renderable_bytes: 65536, artifact_refs: 11, authority_nodes: 48}
      spec: {harness: claude, model: claude-opus-4-8, model_policy: explicit, effort: high, timeout: 30m}
      implementation: {harness: claude, model: claude-opus-4-8, model_policy: explicit, effort: high, timeout: 4h}
      review: {execution: mcp, timeout: 1h, fallback_model: claude-sonnet-4-8, fallback_harness: claude}
    review:
      seats:
        - {model: claude-opus-4-8, harness: claude, effort: high}
  - name: gpt-setup
    refresh_review: delta
    execution_settings: *gpt_settings
    review: *gpt_review
`

func writeMultiSetupRoundTripFixture(t *testing.T) string {
	t.Helper()
	var harnesses []config.Harness
	for _, template := range config.HarnessTemplates() {
		if template.Harness.Name == "codex" || template.Harness.Name == "claude" {
			harnesses = append(harnesses, template.Harness)
		}
	}
	if len(harnesses) != 2 {
		t.Fatalf("expected codex and claude harness templates, got %d", len(harnesses))
	}
	block, err := yaml.Marshal(harnesses)
	if err != nil {
		t.Fatal(err)
	}
	indented := "  " + strings.ReplaceAll(strings.TrimRight(string(block), "\n"), "\n", "\n  ")
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, []byte(strings.Replace(multiSetupRoundTripConfig, "HARNESSES", indented, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = config.Load(path); err != nil {
		t.Fatalf("fixture must load: %v", err)
	}
	return path
}

// readRawConfig decodes the file without config normalization, so the
// comparison sees exactly what `config set` persisted.
func readRawConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func setRawConfigValue(t *testing.T, document map[string]any, value any, path ...string) {
	t.Helper()
	current := document
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("raw config path %v: %q is not a mapping", path, key)
		}
		current = next
	}
	current[path[len(path)-1]] = value
}

func rawSetup(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	setups, _ := document["setups"].([]any)
	for _, entry := range setups {
		setup, _ := entry.(map[string]any)
		if setup["name"] == name {
			return setup
		}
	}
	t.Fatalf("raw config has no setup %q", name)
	return nil
}

// Regression for the 2026-09-29 devbox report: `conveyor config set` on a
// multi-setup file must change only the targeted field of the targeted setup
// (and its default-setup projection) and preserve every other persisted value
// (req-execution-configuration AC-10.2, AC-10.6).
func TestConfigSetRoundTripIsLosslessForMultiSetupFile(t *testing.T) {
	edits := []struct{ key, field, value string }{
		{"execution.implement.harness", "harness", "claude"},
		{"execution.implement.model", "model", "claude-opus-4-8"},
		{"execution.implement.effort", "effort", "high"},
	}
	for _, test := range []struct {
		name string
		set  func(path, key, value string) error
	}{
		{"bare default-setup keys", func(path, key, value string) error {
			return setLocalExecutionField(path, "demo", key, value)
		}},
		{"explicit --setup gpt-setup", func(path, key, value string) error {
			return setNamedLocalExecutionField(path, "gpt-setup", key, value)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMultiSetupRoundTripFixture(t)
			want := readRawConfig(t, path)
			for _, edit := range edits {
				if err := test.set(path, edit.key, edit.value); err != nil {
					t.Fatalf("set %s: %v", edit.key, err)
				}
				setRawConfigValue(t, rawSetup(t, want, "gpt-setup"), edit.value, "execution_settings", "implementation", edit.field)
				setRawConfigValue(t, want, edit.value, "execution_settings", "implementation", edit.field)
				assertRawConfigEqual(t, path, want)
			}
			got := readRawConfig(t, path)
			if !reflect.DeepEqual(got, want) {
				wantText, _ := yaml.Marshal(want)
				gotText, _ := yaml.Marshal(got)
				t.Fatalf("config set was lossy\n--- want\n%s\n--- got\n%s", wantText, gotText)
			}
		})
	}
}

func assertRawConfigEqual(t *testing.T, path string, want map[string]any) {
	t.Helper()
	got := readRawConfig(t, path)
	if !reflect.DeepEqual(got, want) {
		wantText, _ := yaml.Marshal(want)
		gotText, _ := yaml.Marshal(got)
		t.Fatalf("authored config changed unexpectedly\n--- want\n%s\n--- got\n%s", wantText, gotText)
	}
}

func TestConfigSetReviewPreservesFallbacks(t *testing.T) {
	for _, name := range []string{"", "gpt-setup", "claude-setup"} {
		for _, field := range []string{"model", "harness", "effort"} {
			t.Run(name+"/"+field, func(t *testing.T) {
				path := writeMultiSetupRoundTripFixture(t)
				want := readRawConfig(t, path)
				target := name
				if target == "" {
					target = "gpt-setup"
				}
				value := map[string]string{"model": "new-review-model", "harness": "claude", "effort": "medium"}[field]
				if err := setNamedLocalExecutionField(path, name, "execution.review."+field, value); err != nil {
					t.Fatal(err)
				}
				setup := rawSetup(t, want, target)
				setup["review"].(map[string]any)["seats"].([]any)[0].(map[string]any)[field] = value
				if target == "gpt-setup" {
					want["review"].(map[string]any)["seats"].([]any)[0].(map[string]any)[field] = value
				}
				assertRawConfigEqual(t, path, want)
			})
		}
	}
}

func TestConfigSetReviewAllowsEmptyUnusedFallback(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "fallback_model: gpt-5.6-terra, fallback_harness: codex", "fallback_model: '', fallback_harness: ''", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := readRawConfig(t, path)
	if err := setLocalExecutionField(path, "demo", "execution.review.model", "updated-review"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
		item["review"].(map[string]any)["seats"].([]any)[0].(map[string]any)["model"] = "updated-review"
	}
	assertRawConfigEqual(t, path, want)
}

func TestNamedDefaultSwitchPreservesAuthoredConfiguration(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	want := readRawConfig(t, path)
	selected := rawSetup(t, want, "claude-setup")
	want["default_setup"] = "claude-setup"
	want["execution_settings"] = selected["execution_settings"]
	want["review"] = selected["review"]
	if err := setDefaultExecutionSetup(path, "claude-setup"); err != nil {
		t.Fatal(err)
	}
	assertRawConfigEqual(t, path, want)
}

func TestWizardEditsPreserveUncapturedAuthoredSettings(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "default wizard", true: "named wizard"}[named], func(t *testing.T) {
			path := writeMultiSetupRoundTripFixture(t)
			want := readRawConfig(t, path)
			local, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			setup, _, _ := namedSetup(local, "gpt-setup")
			state := &executionWizardState{}
			prefillExecutionWizard(state, setup)
			state.choices.Implement.Model = "wizard-model"
			if named {
				err = writeNamedExecutionSetup(path, local, "demo", "gpt-setup", state.choices, local.Harnesses, true)
			} else {
				err = writeUpdatedLocalExecutionConfig(path, local, state.choices, local.Harnesses)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
				setRawConfigValue(t, item, "wizard-model", "execution_settings", "implementation", "model")
			}
			assertRawConfigEqual(t, path, want)
		})
	}
}

func TestConfigSetPreservesCommentsOrderAndAliases(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "workspace: demo", "# operator configuration\nworkspace: demo # workspace comment", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setLocalExecutionField(path, "demo", "execution.implement.effort", "high"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"# operator configuration", "workspace: demo # workspace comment", "&gpt_settings", "*gpt_settings", "&gpt_review", "*gpt_review"} {
		if !strings.Contains(string(got), text) {
			t.Errorf("lost %q in\n%s", text, got)
		}
	}
	var original, updated yaml.Node
	_ = yaml.Unmarshal(data, &original)
	_ = yaml.Unmarshal(got, &updated)
	for i := 0; i < len(original.Content[0].Content); i += 2 {
		if original.Content[0].Content[i].Value != updated.Content[0].Content[i].Value {
			t.Fatal("top-level key order changed")
		}
	}
}

func TestConfigSetValidationFailurePreservesOriginal(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	original, _ := os.ReadFile(path)
	if err := setLocalExecutionField(path, "demo", "execution.implement.timeout", "1s"); err == nil {
		t.Fatal("invalid timeout accepted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("validation failure changed file")
	}
	if err := setLocalExecutionField(path, "demo", "execution.verify_concurrency", "3"); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	_ = yaml.Unmarshal(original, &want)
	setRawConfigValue(t, want, 3, "execution", "verify_concurrency")
	assertRawConfigEqual(t, path, want)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestConfigSetSharedAliasesDoNotChangeOtherSetups(t *testing.T) {
	for _, name := range []string{"gpt-setup", "shared-setup"} {
		t.Run(name, func(t *testing.T) {
			path := writeMultiSetupRoundTripFixture(t)
			data, _ := os.ReadFile(path)
			data = append(data, []byte("  - name: shared-setup\n    execution_settings: *gpt_settings\n    review: *gpt_review\n")...)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			want := readRawConfig(t, path)
			if err := setNamedLocalExecutionField(path, name, "execution.spec.model", "alias-model"); err != nil {
				t.Fatal(err)
			}
			setRawConfigValue(t, rawSetup(t, want, name), "alias-model", "execution_settings", "spec", "model")
			if name == "gpt-setup" {
				setRawConfigValue(t, want, "alias-model", "execution_settings", "spec", "model")
			}
			assertRawConfigEqual(t, path, want)
		})
	}
}

func TestConfigSetMergeOverridesPreserveSource(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "execution_settings: *gpt_settings", "execution_settings:\n      <<: *gpt_settings", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := readRawConfig(t, path)
	if err := setLocalExecutionField(path, "demo", "execution.implement.model", "merge-model"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
		setRawConfigValue(t, item, "merge-model", "execution_settings", "implementation", "model")
	}
	assertRawConfigEqual(t, path, want)
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "<<:") {
		t.Fatal("merge key lost")
	}
}

func TestNamedSeatMutationsPreserveAuthoredContent(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	want := readRawConfig(t, path)
	seat := config.ReviewSeat{Harness: "claude", Model: "added-seat", Effort: "medium"}
	if err := mutateNamedReviewSeats(path, "gpt-setup", func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) { return append(seats, seat), nil }); err != nil {
		t.Fatal(err)
	}
	for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
		panel := item["review"].(map[string]any)
		panel["seats"] = append(panel["seats"].([]any), map[string]any{"harness": "claude", "model": "added-seat", "effort": "medium"})
	}
	assertRawConfigEqual(t, path, want)
	if err := mutateNamedReviewSeats(path, "gpt-setup", func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
		return []config.ReviewSeat{seats[1], seats[0]}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
		panel := item["review"].(map[string]any)
		seats := panel["seats"].([]any)
		panel["seats"] = []any{seats[1], seats[0]}
	}
	assertRawConfigEqual(t, path, want)
}

func TestConfigSetDoesNotIntroduceOmittedProjection(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	want := readRawConfig(t, path)
	delete(want, "execution_settings")
	delete(want, "review")
	data, _ := yaml.Marshal(want)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setLocalExecutionField(path, "demo", "execution.implement.effort", "high"); err != nil {
		t.Fatal(err)
	}
	setRawConfigValue(t, rawSetup(t, want, "gpt-setup"), "high", "execution_settings", "implementation", "effort")
	assertRawConfigEqual(t, path, want)
}

func TestConfigSetLegacySingletonRemainsSingleton(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	want := readRawConfig(t, path)
	delete(want, "setups")
	delete(want, "default_setup")
	data, _ := yaml.Marshal(want)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setLocalExecutionField(path, "demo", "execution.spec.model", "legacy-model"); err != nil {
		t.Fatal(err)
	}
	setRawConfigValue(t, want, "legacy-model", "execution_settings", "spec", "model")
	assertRawConfigEqual(t, path, want)
}

func TestConfigSetClearsMergeInheritedEffort(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "implementation: {harness: codex, model: gpt-5.6-sol, model_policy: explicit, effort: medium, timeout: 4h}", "implementation: {<<: {harness: codex, model: gpt-5.6-sol, model_policy: explicit, effort: medium, timeout: 4h}}", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := readRawConfig(t, path)
	if err := setLocalExecutionField(path, "demo", "execution.implement.effort", ""); err != nil {
		t.Fatal(err)
	}
	for _, item := range []map[string]any{want, rawSetup(t, want, "gpt-setup")} {
		setRawConfigValue(t, item, "", "execution_settings", "implementation", "effort")
	}
	assertRawConfigEqual(t, path, want)
}

func TestConfigSetRootMergeRetainsNamedSetupSelection(t *testing.T) {
	path := writeMultiSetupRoundTripFixture(t)
	want := readRawConfig(t, path)
	data, _ := yaml.Marshal(want)
	data = []byte("<<:\n  " + strings.ReplaceAll(strings.TrimSpace(string(data)), "\n", "\n  ") + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setNamedLocalExecutionField(path, "claude-setup", "execution.implement.model", "root-merge-model"); err != nil {
		t.Fatal(err)
	}
	setRawConfigValue(t, rawSetup(t, want, "claude-setup"), "root-merge-model", "execution_settings", "implementation", "model")
	assertRawConfigEqual(t, path, want)
}
