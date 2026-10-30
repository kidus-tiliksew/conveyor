package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// stubNamedSetupWizard runs the wizard with a UI that applies choose to the
// prefilled choices and confirms them.
func stubNamedSetupWizard(t *testing.T, choose func(*localExecutionChoices)) {
	t.Helper()
	previousTerminal := wizardTerminal
	previousUI := runExecutionWizardUI
	wizardTerminal = func(io.Reader) bool { return true }
	runExecutionWizardUI = func(model *executionWizardModel, _ io.Reader, _ io.Writer) (*executionWizardModel, error) {
		model.state.acceptDefaults = false
		model.state.confirmSummary = true
		model.state.seatAction = "continue"
		choose(&model.state.choices)
		model.completed = true
		return model, nil
	}
	t.Cleanup(func() {
		wizardTerminal = previousTerminal
		runExecutionWizardUI = previousUI
	})
}

func namedSetupFields(pairs ...string) []namedSetupFieldValue {
	var fields []namedSetupFieldValue
	for index := 0; index+1 < len(pairs); index += 2 {
		item := namedSetupFieldValue{flag: pairs[index], value: pairs[index+1]}
		if stage, field, ok := strings.Cut(pairs[index], "-"); ok && pairs[index] != namedSetupWorktreeLocationFlag {
			item.stage, item.field = stage, field
		}
		fields = append(fields, item)
	}
	return fields
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// req-agent-guided-setup AC-5.2: create and edit with field flags write the
// same document the wizard writes for the same choices.
func TestNamedSetupFieldFlagsWriteWhatTheWizardWrites(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	writeProbeFixture(t, directory, "claude", "claude 2.0.0", true)
	t.Setenv("PATH", directory)
	t.Setenv("HOME", directory)
	wizardPath := filepath.Join(directory, "wizard", "conveyor.yaml")
	flagPath := filepath.Join(directory, "flags", "conveyor.yaml")

	stubNamedSetupWizard(t, func(choices *localExecutionChoices) {
		choices.Spec = localStageChoice{Harness: "claude", Model: "sonnet", Effort: "medium", Timeout: choices.Spec.Timeout}
		choices.Implement.Model = "gpt-5.6-flag"
		choices.ReviewSeats[0] = localStageChoice{Harness: "claude", Model: "opus", Effort: "low"}
		choices.WorktreeLocation = "machine"
	})
	if err := runNamedExecutionSetupWizard(t.Context(), strings.NewReader(""), io.Discard, wizardPath, "demo", "primary", false); err != nil {
		t.Fatal(err)
	}
	createFlags := namedSetupFields(
		"spec-harness", "claude", "spec-model", "sonnet", "spec-effort", "medium",
		"implement-model", "gpt-5.6-flag",
		"review-harness", "claude", "review-model", "opus", "review-effort", "low",
		"worktree-location", "machine",
	)
	var output bytes.Buffer
	if err := runNamedExecutionSetupFlags(t.Context(), &output, flagPath, "demo", "primary", false, createFlags); err != nil {
		t.Fatal(err)
	}
	if got, want := readFileOrEmpty(t, flagPath), readFileOrEmpty(t, wizardPath); got != want || want == "" {
		t.Fatalf("flag create wrote\n%s\nwizard create wrote\n%s", got, want)
	}
	if !strings.Contains(output.String(), "Saved local execution setup primary") {
		t.Fatalf("output = %q", output.String())
	}

	stubNamedSetupWizard(t, func(choices *localExecutionChoices) {
		choices.Implement = localStageChoice{Harness: "claude", Model: suggestedHarnessModel("claude", "implement"), Effort: choices.Implement.Effort, Timeout: choices.Implement.Timeout}
		choices.Verify.Effort = "low"
		choices.ReviewSeats[0].Model = "opus-edited"
	})
	if err := runNamedExecutionSetupWizard(t.Context(), strings.NewReader(""), io.Discard, wizardPath, "demo", "primary", true); err != nil {
		t.Fatal(err)
	}
	editFlags := namedSetupFields("implement-harness", "claude", "verify-effort", "low", "review-model", "opus-edited")
	if err := runNamedExecutionSetupFlags(t.Context(), io.Discard, flagPath, "demo", "primary", true, editFlags); err != nil {
		t.Fatal(err)
	}
	if got, want := readFileOrEmpty(t, flagPath), readFileOrEmpty(t, wizardPath); got != want {
		t.Fatalf("flag edit wrote\n%s\nwizard edit wrote\n%s", got, want)
	}
	local, err := config.Load(flagPath)
	if err != nil {
		t.Fatal(err)
	}
	setup, _, err := namedSetup(local, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if setup.ExecutionSettings.Spec.Model != "sonnet" || setup.ExecutionSettings.Implementation.Harness != "claude" || setup.ExecutionSettings.Verify.Effort != "low" || setup.Review.Seats[0].Model != "opus-edited" {
		t.Fatalf("edited setup = %+v seats %+v", setup.ExecutionSettings, setup.Review.Seats)
	}
}

// Field flags run the wizard's harness probe and refuse a harness that fails
// it, an unsupported harness, and invalid values, naming each flag and
// leaving the file unchanged.
func TestNamedSetupFieldFlagsRefuseInvalidFieldsWithoutWriting(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	writeProbeFixture(t, directory, "claude", "authentication required", false)
	t.Setenv("PATH", directory)
	t.Setenv("HOME", directory)
	path := filepath.Join(directory, "conveyor.yaml")

	err := runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", false, namedSetupFields(
		"spec-harness", "claude", "implement-harness", "other", "verify-effort", "extreme",
		"review-model", "", "worktree-location", "elsewhere",
	))
	if err == nil {
		t.Fatal("invalid field flags were accepted")
	}
	for _, want := range []string{
		"--spec-harness: harness \"claude\" failed validation probe: authentication required",
		"--implement-harness: unsupported harness \"other\"",
		"--verify-effort: effort must be",
		"--review-model: model is required",
		"--worktree-location:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if readFileOrEmpty(t, path) != "" {
		t.Fatal("refused create wrote the local execution config")
	}

	if err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", false, namedSetupFields("worktree-location", "machine")); err != nil {
		t.Fatal(err)
	}
	before := readFileOrEmpty(t, path)
	err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", true, namedSetupFields("implement-harness", "claude", "spec-effort", "medium"))
	if err == nil || !strings.Contains(err.Error(), "--implement-harness: harness \"claude\" failed validation probe") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("probe failure error = %v", err)
	}
	if after := readFileOrEmpty(t, path); after != before {
		t.Fatalf("refused edit changed the file:\n%s", after)
	}
}

// create refuses an existing name and edit refuses a missing name or file,
// as the wizard does, without writing.
func TestNamedSetupFieldFlagsKeepCreateAndEditNameRules(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	t.Setenv("HOME", directory)
	path := filepath.Join(directory, "conveyor.yaml")

	err := runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", true, namedSetupFields("spec-model", "x"))
	if err == nil || !strings.Contains(err.Error(), `load named setup "primary"`) || readFileOrEmpty(t, path) != "" {
		t.Fatalf("edit without a file: err=%v", err)
	}
	if err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", false, namedSetupFields("worktree-location", "machine")); err != nil {
		t.Fatal(err)
	}
	before := readFileOrEmpty(t, path)
	err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "primary", false, namedSetupFields("spec-model", "x"))
	if err == nil || !strings.Contains(err.Error(), `setup "primary" already exists`) {
		t.Fatalf("create of an existing name: err=%v", err)
	}
	err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "missing", true, namedSetupFields("spec-model", "x"))
	if err == nil || !strings.Contains(err.Error(), `unknown setup "missing"`) {
		t.Fatalf("edit of a missing name: err=%v", err)
	}
	err = runNamedExecutionSetupFlags(t.Context(), io.Discard, path, "demo", "../escape", false, namedSetupFields("spec-model", "x"))
	if err == nil || !strings.Contains(err.Error(), "path-safe") {
		t.Fatalf("create of an unsafe name: err=%v", err)
	}
	if after := readFileOrEmpty(t, path); after != before {
		t.Fatalf("refused acts changed the file:\n%s", after)
	}
}

// Both commands register one flag per wizard field, named after the
// `conveyor config set execution.<stage>.<field>` key, and dispatch to the
// flag path only when a field flag is given.
func TestSetupCreateAndEditRegisterConfigSetFieldFlags(t *testing.T) {
	command := setupCmd()
	for _, name := range []string{"create", "edit"} {
		sub, _, err := command.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{"spec", "implement", "verify", "review"} {
			for _, field := range []string{"harness", "model", "effort"} {
				if sub.Flags().Lookup(stage+"-"+field) == nil {
					t.Fatalf("%s lacks --%s-%s for execution.%s.%s", name, stage, field, stage, field)
				}
			}
		}
		if sub.Flags().Lookup("worktree-location") == nil {
			t.Fatalf("%s lacks --worktree-location", name)
		}
	}
	flags := namedSetupFieldFlags{}
	probe := setupCmd()
	flags.register(probe)
	if err := probe.ParseFlags([]string{"--config", "x.yaml"}); err != nil {
		t.Fatal(err)
	}
	if given := flags.given(probe); len(given) != 0 {
		t.Fatalf("--config alone selected the flag path: %+v", given)
	}
	if err := probe.ParseFlags([]string{"--review-effort", " high ", "--worktree-location", "machine"}); err != nil {
		t.Fatal(err)
	}
	given := flags.given(probe)
	if len(given) != 2 || given[0] != (namedSetupFieldValue{flag: "review-effort", stage: "review", field: "effort", value: "high"}) || given[1] != (namedSetupFieldValue{flag: "worktree-location", value: "machine"}) {
		t.Fatalf("given = %+v", given)
	}
}
