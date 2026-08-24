package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"gopkg.in/yaml.v3"
)

func healthyDetections(harnesses ...config.Harness) []detectedHarness {
	detected := make([]detectedHarness, len(harnesses))
	for index, harness := range harnesses {
		detected[index] = detectedHarness{Harness: harness, Probe: core.HarnessProbe{Harness: harness.Name, Healthy: true, Fingerprint: "test", Message: "test harness"}}
	}
	return detected
}

func driveExecutionWizard(t *testing.T, model *executionWizardModel, message tea.Msg) *executionWizardModel {
	t.Helper()
	updated, command := model.Update(message)
	model = updated.(*executionWizardModel)
	for command != nil && !model.completed && !model.cancelled {
		updated, command = model.Update(command())
		model = updated.(*executionWizardModel)
	}
	return model
}

func TestDetectLocalHarnessesOffersOnlyHealthyPresentTemplates(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	writeProbeFixture(t, directory, "grok", "broken", false)
	t.Setenv("PATH", directory)

	harnesses := detectLocalHarnesses(t.Context())
	if len(harnesses) != 1 || harnesses[0].Name != "codex" {
		t.Fatalf("detected harnesses = %+v", harnesses)
	}
}

func TestExecutionWizardDeclineWritesNothing(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "local.yaml")
	previousTerminal := wizardTerminal
	wizardTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { wizardTerminal = previousTerminal })

	var output bytes.Buffer
	err := runExecutionSetupWizard(t.Context(), strings.NewReader("\x1b"), &output, path, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled wizard wrote %s: %v", path, err)
	}
}

func TestExecutionWizardModelRoundTripWritesRunValidConfig(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	state := newExecutionWizardState(healthyDetections(harness), nil)
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", state.choices, []config.Harness{harness}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateWorkerConfig(workerservice.WorkerConfig{WorkspaceDocument: loaded.WorkspaceDocument()}); err != nil {
		t.Fatalf("conveyor run rejected wizard config: %v", err)
	}
	for _, order := range []core.WorkOrder{
		{ID: "implement-1", Stage: core.StageImplement},
		{ID: "review-1", Stage: core.StageReview, ReviewSeat: 1},
	} {
		selected, selectErr := selectLocalRunDispatch(workerservice.DispatchOrder{Order: order}, loaded)
		if selectErr != nil {
			t.Fatalf("conveyor run could not select %s dispatch: %v", order.Stage, selectErr)
		}
		if selected.Harness.Name != harness.Name || selected.Model == "" || selected.Dispatch != "run" || selected.Auth != "user" {
			t.Fatalf("selected %s dispatch = %+v", order.Stage, selected)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "credential:") || strings.Contains(lower, "api_token:") || strings.Contains(lower, "authorization:") {
		t.Fatalf("config contains credential-like content:\n%s", data)
	}
}

func TestExecutionWizardDefaultsFastPathCompletesFromOneConfirmation(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	harness := config.HarnessTemplates()[0].Harness
	detected := healthyDetections(harness)
	model := newExecutionWizardModel(nil, detected)
	model = driveExecutionWizard(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	if !model.completed || !model.state.acceptDefaults || len(model.state.choices.ReviewSeats) != 1 {
		t.Fatalf("completed=%v defaults=%v choices=%+v", model.completed, model.state.acceptDefaults, model.state.choices)
	}
}

func TestExecutionWizardUsesGroupedHuhFormAndPreservesState(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	detected := healthyDetections(harness)
	state := newExecutionWizardState(detected, nil)
	state.acceptDefaults = false
	state.choices.Spec.Model = "operator-model"
	state.choices.Implement.Model = "later-model"
	model := newExecutionWizardModel(state, detected)
	view := model.View()
	if !strings.Contains(view, "Local execution setup") || model.form == nil {
		t.Fatalf("huh form view = %q", view)
	}
	model.form.NextGroup()
	model.form.NextGroup()
	model.form.PrevGroup()
	if state.choices.Spec.Model != "operator-model" || state.choices.Implement.Model != "later-model" {
		t.Fatalf("back navigation lost values: %+v", state.choices)
	}
}

func TestExecutionWizardDirectSeatActionsAddRemoveAndReorder(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	state := newExecutionWizardState(healthyDetections(harness), []localStageChoice{
		{Harness: harness.Name, Model: "first", Effort: "high"},
		{Harness: harness.Name, Model: "second", Effort: "medium"},
	})
	state.seatIndex, state.seatAction = 1, "up"
	applySeatAction(state)
	if state.choices.ReviewSeats[0].Model != "second" {
		t.Fatalf("move seats=%+v", state.choices.ReviewSeats)
	}
	state.seatAction = "add"
	applySeatAction(state)
	if len(state.choices.ReviewSeats) != 3 {
		t.Fatalf("add seats=%+v", state.choices.ReviewSeats)
	}
	state.seatAction = "remove"
	applySeatAction(state)
	if len(state.choices.ReviewSeats) != 2 {
		t.Fatalf("remove seats=%+v", state.choices.ReviewSeats)
	}
	document := localExecutionDocument("demo", state.choices, []config.Harness{harness})
	if len(document.Review.Seats) != 2 || document.Review.Seats[0].Model != "second" {
		t.Fatalf("document seats=%+v", document.Review.Seats)
	}
}

func TestRunAndWorkerShareLocalSetupLoaderAndResolution(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	state := newExecutionWizardState(healthyDetections(harness), nil)
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", state.choices, []config.Harness{harness}); err != nil {
		t.Fatal(err)
	}
	setup, err := loadLocalExecutionSetup(path)
	if err != nil {
		t.Fatal(err)
	}
	item := workerservice.DispatchOrder{Order: core.WorkOrder{
		ID: "shared-loader", Stage: core.StageImplement, RequiredHarness: "server-harness", RequiredModel: "server-model",
		RequiredHarnessConfig: &core.HarnessSnapshot{Name: "server-harness", Command: []string{"server-agent"}},
	}}
	run, err := selectLocalRunDispatch(item, setup.Config)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := selectLocalWorkerDispatch(item, setup.Config, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness.Name != worker.Harness.Name || run.Model != worker.Model || run.Effort != worker.Effort || !reflect.DeepEqual(run.EffortArgv, worker.EffortArgv) {
		t.Fatalf("run=%+v worker=%+v", run, worker)
	}
	if setup.FirstActivityTimeout <= 0 || len(setup.WorkerDocument.Harnesses) != 1 || setup.WorkerDocument.Harnesses[0].Name != run.Harness.Name {
		t.Fatalf("setup=%+v run=%+v", setup, run)
	}
}

func TestSharedLocalSetupLoaderRejectsInvalidResumeCommand(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	state := newExecutionWizardState(healthyDetections(harness), nil)
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", state.choices, []config.Harness{harness}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	harnesses, ok := document["harnesses"].([]any)
	if !ok || len(harnesses) == 0 {
		t.Fatalf("generated harnesses = %#v", document["harnesses"])
	}
	harnessEntry, ok := harnesses[0].(map[string]any)
	if !ok {
		t.Fatalf("generated harness = %#v", harnesses[0])
	}
	harnessEntry["resume_command"] = []string{"--resume", "missing-session-placeholder"}
	raw, err = yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadLocalExecutionSetup(path); err == nil || !strings.Contains(err.Error(), "harnesses[0].resume_command must contain exactly one {session_id}") {
		t.Fatalf("loader error = %v", err)
	}
}

func TestExecutionSetupCommandIsDistinctFromTaskSetup(t *testing.T) {
	command := configCmd()
	resolved, _, err := command.Find([]string{"init-execution"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.CommandPath() != "config init-execution" || !strings.Contains(resolved.Long, "conveyor task setup") || resolved.Flags().Lookup("defaults") == nil {
		t.Fatalf("init command path=%q long=%q", resolved.CommandPath(), resolved.Long)
	}
}

func TestExecutionWizardPreservesExistingLocalConfiguration(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	source, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "conveyor.yaml")
	if err = os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	previousTerminal := wizardTerminal
	wizardTerminal = func(io.Reader) bool { return true }
	previousUI := runExecutionWizardUI
	runExecutionWizardUI = func(model *executionWizardModel, _ io.Reader, _ io.Writer) (*executionWizardModel, error) {
		model.completed = true
		return model, nil
	}
	t.Cleanup(func() {
		wizardTerminal = previousTerminal
		runExecutionWizardUI = previousUI
	})

	var output bytes.Buffer
	if err = runExecutionSetupWizard(t.Context(), strings.NewReader(""), &output, path, "ignored-workspace"); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Workspace != before.Workspace || after.PackDir != before.PackDir || !reflect.DeepEqual(after.Repos, before.Repos) || len(after.Setups) != len(before.Setups) {
		t.Fatalf("wizard changed unrelated config: before=%+v after=%+v", before, after)
	}
	if after.ExecutionSettings == nil || after.ExecutionSettings.Spec.Harness != "codex" || after.ExecutionSettings.Implementation.Harness != "codex" {
		t.Fatalf("wizard selections were not applied: %+v", after.ExecutionSettings)
	}
}

func TestExecutionWizardUsesDetectionProbeWithoutDiscardingAnswers(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "conveyor.yaml")
	previousTerminal := wizardTerminal
	wizardTerminal = func(io.Reader) bool { return true }
	previousUI := runExecutionWizardUI
	runExecutionWizardUI = func(model *executionWizardModel, _ io.Reader, _ io.Writer) (*executionWizardModel, error) {
		model.state.choices.Spec.Model = "captured-model"
		if err := os.WriteFile(filepath.Join(directory, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			return nil, err
		}
		model.completed = true
		return model, nil
	}
	t.Cleanup(func() {
		wizardTerminal = previousTerminal
		runExecutionWizardUI = previousUI
	})

	if err := runExecutionSetupWizard(t.Context(), strings.NewReader(""), &bytes.Buffer{}, path, "demo"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExecutionSettings.Spec.Model != "captured-model" {
		t.Fatalf("captured answers were lost: %+v", loaded.ExecutionSettings.Spec)
	}
}

func TestExecutionSetupDefaultsWritesWithoutTerminal(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "conveyor.yaml")
	if err := runExecutionSetupDefaults(t.Context(), io.Discard, path, "demo"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExecutionSettings.Spec.Model != suggestedHarnessModel("codex", "spec") || len(loaded.Review.Seats) != 1 {
		t.Fatalf("defaults setup = %+v seats=%+v", loaded.ExecutionSettings, loaded.Review.Seats)
	}
}

func TestExecutionWizardSummaryDeclineReturnsWithValuesIntact(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "conveyor.yaml")
	previousTerminal := wizardTerminal
	wizardTerminal = func(io.Reader) bool { return true }
	previousUI := runExecutionWizardUI
	calls := 0
	runExecutionWizardUI = func(model *executionWizardModel, _ io.Reader, _ io.Writer) (*executionWizardModel, error) {
		calls++
		model.state.acceptDefaults = false
		model.state.choices.Implement.Model = "preserved-after-summary-decline"
		model.state.confirmSummary = calls > 1
		model.completed = true
		return model, nil
	}
	t.Cleanup(func() {
		wizardTerminal = previousTerminal
		runExecutionWizardUI = previousUI
	})
	if err := runExecutionSetupWizard(t.Context(), strings.NewReader(""), io.Discard, path, "demo"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || loaded.ExecutionSettings.Implementation.Model != "preserved-after-summary-decline" {
		t.Fatalf("calls=%d implementation=%+v", calls, loaded.ExecutionSettings.Implementation)
	}
}

func TestExecutionWizardUnhealthyHarnessValidationPreservesChoices(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	detected := []detectedHarness{{Harness: harness, Probe: core.HarnessProbe{Harness: harness.Name, Message: "authentication required"}}}
	state := &executionWizardState{choices: localExecutionChoices{Spec: localStageChoice{Harness: harness.Name, Model: "kept-model"}}}
	err := harnessProbeValidator(detected)(harness.Name)
	if err == nil || !strings.Contains(err.Error(), "authentication required") || state.choices.Spec.Model != "kept-model" {
		t.Fatalf("error=%v state=%+v", err, state.choices.Spec)
	}
}

func TestExecutionWizardNonTerminalPointsAtConfigTwin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	err := runExecutionSetupWizard(t.Context(), strings.NewReader(""), &bytes.Buffer{}, path, "demo")
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") || !strings.Contains(err.Error(), "config set execution.<stage>.<field>") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigSetTwinCoversEveryWizardField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	want := map[string]string{}
	for _, stage := range []string{"spec", "implement", "review"} {
		for _, field := range []struct{ name, value string }{
			{"harness", "claude"}, {"model", "chosen-" + stage}, {"effort", "medium"}, {"timeout", "45m"},
		} {
			key := "execution." + stage + "." + field.name
			if err := setLocalExecutionField(path, "demo", key, field.value); err != nil {
				t.Fatalf("set %s: %v", key, err)
			}
			want[key] = field.value
		}
	}
	var output bytes.Buffer
	if err := printLocalExecutionConfig(&output, path); err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if !strings.Contains(output.String(), key+"\t"+value+"\tstored file "+path) {
			t.Errorf("config list missing %s=%s:\n%s", key, value, output.String())
		}
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("config set twin did not produce a valid config: %v", err)
	}
}

func TestConfigSetPreservesUnrelatedLocalConfiguration(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = setLocalExecutionField(path, "demo", "execution.spec.model", "updated-spec-model"); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.PackDir != before.PackDir || !reflect.DeepEqual(after.Repos, before.Repos) || len(after.Setups) != len(before.Setups) || len(after.Harnesses) != len(before.Harnesses) {
		t.Fatalf("unrelated config changed: before=%+v after=%+v", before, after)
	}
	if after.ExecutionSettings == nil || after.ExecutionSettings.Spec.Model != "updated-spec-model" {
		t.Fatalf("spec model was not updated: %+v", after.ExecutionSettings)
	}
}

func TestInteractiveDependenciesAreConfinedToConveyorCLI(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, relative := range []string{"cmd/conveyord", "internal/store"} {
		err := filepath.WalkDir(filepath.Join(root, relative), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, dependency := range []string{"charmbracelet/bubbletea", "charmbracelet/bubbles", "charmbracelet/lipgloss"} {
				if strings.Contains(string(data), dependency) {
					t.Errorf("interactive dependency %q escaped cmd/conveyor: %s", dependency, path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWizardPathHasNoNetworkClient(t *testing.T) {
	// The signature and imports lock the wizard to local inputs. A future server
	// dependency must be an explicit contract change rather than an unnoticed
	// upload of setup content.
	type localWizard func(context.Context, io.Reader, io.Writer, string, string) error
	var wizard localWizard = runExecutionSetupWizard
	if wizard == nil {
		t.Fatal("local wizard unavailable")
	}
	data, err := os.ReadFile("execution_setup.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"net/http"`, "newClient("} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("wizard path contains network client dependency %q", forbidden)
		}
	}
}

func writeProbeFixture(t *testing.T, directory, name, output string, healthy bool) {
	t.Helper()
	exit := "0"
	if !healthy {
		exit = "1"
	}
	contents := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\nexit " + exit + "\n"
	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
