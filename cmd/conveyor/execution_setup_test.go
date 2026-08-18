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
	model := newExecutionWizardModel([]config.Harness{harness})
	for model.field < len(model.fields) {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(executionWizardModel)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", model.choices, []config.Harness{harness}); err != nil {
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

func TestExecutionWizardUsesEditableModelPlaceholder(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	model := newExecutionWizardModel([]config.Harness{harness})
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(executionWizardModel)

	if model.fields[model.field].name != "model" || model.input.Value() != "" || model.input.Placeholder != "gpt-5.6-sol" || !model.input.Focused() {
		t.Fatalf("model input = field %q value %q placeholder %q focused %v", model.fields[model.field].name, model.input.Value(), model.input.Placeholder, model.input.Focused())
	}
	view := model.View()
	for _, text := range []string{"Step 2 of 12", "suggested default shown dimmed", "type any model to replace it"} {
		if !strings.Contains(view, text) {
			t.Fatalf("model view missing %q:\n%s", text, view)
		}
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("operator-model")})
	model = updated.(executionWizardModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(executionWizardModel)
	if model.choices.Spec.Model != "operator-model" {
		t.Fatalf("spec model = %q", model.choices.Spec.Model)
	}
}

func TestExecutionWizardHarnessSelectionRefreshesSuggestion(t *testing.T) {
	templates := config.HarnessTemplates()
	var codex, claude config.Harness
	for _, template := range templates {
		switch template.Harness.Name {
		case "codex":
			codex = template.Harness
		case "claude":
			claude = template.Harness
		}
	}
	model := newExecutionWizardModel([]config.Harness{codex, claude})
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(executionWizardModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(executionWizardModel)
	if model.choices.Spec.Harness != "claude" || model.input.Placeholder != "opus" {
		t.Fatalf("choice = %+v placeholder = %q", model.choices.Spec, model.input.Placeholder)
	}
	if strings.Contains(suggestedHarnessModel("claude", "spec"), "4.1") {
		t.Fatalf("claude suggestion is pinned: %q", suggestedHarnessModel("claude", "spec"))
	}
}

func TestExecutionWizardRendersStyledValidationAndKeyHelp(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	model := newExecutionWizardModel([]config.Harness{harness})
	for model.field < 2 {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(executionWizardModel)
	}
	if view := model.View(); !strings.Contains(view, "↑/↓ select • enter confirm • esc cancel") || !strings.Contains(view, "› high") {
		t.Fatalf("selector view lacks focus/help:\n%s", view)
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(executionWizardModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("not-a-duration")})
	model = updated.(executionWizardModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(executionWizardModel)
	want := wizardValidationStyle.Render("! timeout must be a positive duration")
	if !strings.Contains(model.View(), want) {
		t.Fatalf("validation is not rendered with wizard style:\n%s", model.View())
	}
}

func TestRunAndWorkerShareLocalSetupLoaderAndResolution(t *testing.T) {
	harness := config.HarnessTemplates()[0].Harness
	model := newExecutionWizardModel([]config.Harness{harness})
	for model.field < len(model.fields) {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(executionWizardModel)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", model.choices, []config.Harness{harness}); err != nil {
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
	model := newExecutionWizardModel([]config.Harness{harness})
	for model.field < len(model.fields) {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(executionWizardModel)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", model.choices, []config.Harness{harness}); err != nil {
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
	if resolved.CommandPath() != "config init-execution" || !strings.Contains(resolved.Long, "conveyor task setup") {
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
	runExecutionWizardUI = func(model executionWizardModel, _ io.Reader, _ io.Writer) (executionWizardModel, error) {
		for model.field < len(model.fields) {
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			model = updated.(executionWizardModel)
		}
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

func TestExecutionWizardRejectsHarnessThatLosesVersionFingerprintBeforeSave(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "conveyor.yaml")
	previousTerminal := wizardTerminal
	wizardTerminal = func(io.Reader) bool { return true }
	previousUI := runExecutionWizardUI
	runExecutionWizardUI = func(model executionWizardModel, _ io.Reader, _ io.Writer) (executionWizardModel, error) {
		for model.field < len(model.fields) {
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			model = updated.(executionWizardModel)
		}
		if err := os.WriteFile(filepath.Join(directory, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			return executionWizardModel{}, err
		}
		return model, nil
	}
	t.Cleanup(func() {
		wizardTerminal = previousTerminal
		runExecutionWizardUI = previousUI
	})

	err := runExecutionSetupWizard(t.Context(), strings.NewReader(""), &bytes.Buffer{}, path, "demo")
	if err == nil || !strings.Contains(err.Error(), "failed validation probe") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), wizardValidationStyle.Render("! harness \"codex\" failed validation probe:")) {
		t.Fatalf("probe validation is not styled: %q", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("failed validation wrote %s: %v", path, statErr)
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
