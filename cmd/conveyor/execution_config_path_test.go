package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

func TestLocalExecutionConfigPathPrecedence(t *testing.T) {
	cwd := t.TempDir()
	configRoot := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return configRoot, nil }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })

	userDefault := filepath.Join(configRoot, "conveyor", localExecutionConfigName)
	assertResolvedExecutionConfig(t, nil, "", userDefault, "user default")
	if err := os.MkdirAll(filepath.Dir(userDefault), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userDefault, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertResolvedExecutionConfig(t, nil, "", userDefault, "user default")

	cwdPath := filepath.Join(cwd, localExecutionConfigName)
	if err := os.WriteFile(cwdPath, []byte("cwd"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertResolvedExecutionConfig(t, nil, "", cwdPath, "working-directory file")

	t.Setenv("CONVEYOR_CONFIG", "env.yaml")
	assertResolvedExecutionConfig(t, nil, "", filepath.Join(cwd, "env.yaml"), "environment CONVEYOR_CONFIG")

	command := &cobra.Command{Use: "test"}
	var flagValue string
	command.Flags().StringVar(&flagValue, "config", "", "")
	if err := command.Flags().Set("config", "flag.yaml"); err != nil {
		t.Fatal(err)
	}
	assertResolvedExecutionConfig(t, command, flagValue, filepath.Join(cwd, "flag.yaml"), "flag")
}

func TestLocalExecutionConfigPathSelectionDoesNotFallThrough(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })

	command := &cobra.Command{Use: "test"}
	var flagValue string
	command.Flags().StringVar(&flagValue, "config", "", "")
	if err := command.Flags().Set("config", "missing.yaml"); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveLocalExecutionConfigPath(command, flagValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loadLocalExecutionSetup(resolved.Path); err == nil || !strings.Contains(err.Error(), resolved.Path) {
		t.Fatalf("explicit missing path error = %v", err)
	}

	directoryPath := t.TempDir()
	if err = command.Flags().Set("config", directoryPath); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolveLocalExecutionConfigPath(command, flagValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loadLocalExecutionSetup(resolved.Path); err == nil || !strings.Contains(err.Error(), resolved.Path) {
		t.Fatalf("explicit unreadable path error = %v", err)
	}
}

func TestLocalExecutionConfigPathIsLazyAndHonorsInheritedFlag(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CONVEYOR_CONFIG", "first.yaml")
	command := runCmd()
	t.Setenv("CONVEYOR_CONFIG", "second.yaml")
	flag := command.Flags().Lookup("config")
	assertResolvedExecutionConfig(t, command, flag.Value.String(), filepath.Join(cwd, "second.yaml"), "environment CONVEYOR_CONFIG")

	setup := setupCmd()
	if err := setup.PersistentFlags().Set("config", "named.yaml"); err != nil {
		t.Fatal(err)
	}
	list := mustFindCommand(t, setup, []string{"list"})
	inherited := list.InheritedFlags().Lookup("config")
	if inherited == nil {
		t.Fatal("setup list did not inherit the config flag")
	}
	assertResolvedExecutionConfig(t, list, inherited.Value.String(), filepath.Join(cwd, "named.yaml"), "flag")
}

func TestUserDefaultExecutionConfigIsFoundFromAnotherWorkingDirectory(t *testing.T) {
	configRoot := t.TempDir()
	firstCWD := t.TempDir()
	secondCWD := t.TempDir()
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return configRoot, nil }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })

	t.Chdir(firstCWD)
	initial, err := resolveLocalExecutionConfigPath(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = setLocalExecutionField(initial.Path, "demo", "execution.spec.model", "cross-directory-model"); err != nil {
		t.Fatal(err)
	}

	t.Chdir(secondCWD)
	rediscovered, err := resolveLocalExecutionConfigPath(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rediscovered != initial {
		t.Fatalf("rediscovered=%+v initial=%+v", rediscovered, initial)
	}
	setup, err := loadLocalExecutionSetup(rediscovered.Path)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Config.ExecutionSettings.Spec.Model != "cross-directory-model" {
		t.Fatalf("spec model = %q", setup.Config.ExecutionSettings.Spec.Model)
	}
}

func TestConfigInitExecutionWritesUserDefaultForOtherConsumers(t *testing.T) {
	configRoot := isolateLocalAuthTest(t)
	firstCWD := t.TempDir()
	secondCWD := t.TempDir()
	binDir := t.TempDir()
	writeProbeFixture(t, binDir, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", binDir)
	t.Setenv("CONVEYOR_CONFIG", "")
	t.Chdir(firstCWD)

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
	command := configCmd()
	command.SetArgs([]string{"init-execution"})
	command.SetIn(strings.NewReader(""))
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configRoot, "conveyor", localExecutionConfigName)
	if !strings.Contains(output.String(), want) {
		t.Fatalf("init output %q does not name %s", output.String(), want)
	}

	t.Chdir(secondCWD)
	for name, consumer := range map[string]*cobra.Command{
		"run":            runCmd(),
		"worker run":     mustFindCommand(t, workerCmd(), []string{"run"}),
		"setup list":     mustFindCommand(t, setupCmd(), []string{"list"}),
		"worker install": workerInstallCmd(),
	} {
		flag := consumer.Flags().Lookup("config")
		if flag == nil {
			flag = consumer.InheritedFlags().Lookup("config")
		}
		if flag == nil {
			t.Fatalf("%s has no config flag", name)
		}
		resolved, err := resolveLocalExecutionConfigPath(consumer, flag.Value.String())
		if err != nil {
			t.Fatalf("%s resolve: %v", name, err)
		}
		if resolved.Path != want || resolved.Source != "user default" {
			t.Fatalf("%s resolved %+v, want %s from user default", name, resolved, want)
		}
		if _, err = loadLocalExecutionSetup(resolved.Path); err != nil {
			t.Fatalf("%s load: %v", name, err)
		}
	}
}

func TestLocalExecutionConfigPathReportsUserConfigError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return "", errors.New("unavailable") }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })
	if _, err := resolveLocalExecutionConfigPath(nil, ""); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func assertResolvedExecutionConfig(t *testing.T, command *cobra.Command, flagValue, wantPath, wantSource string) {
	t.Helper()
	resolved, err := resolveLocalExecutionConfigPath(command, flagValue)
	if err != nil {
		t.Fatal(err)
	}
	wantPath, err = filepath.Abs(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path != wantPath || resolved.Source != wantSource {
		t.Fatalf("resolved=%+v want path=%q source=%q", resolved, wantPath, wantSource)
	}
}

func mustFindCommand(t *testing.T, root *cobra.Command, args []string) *cobra.Command {
	t.Helper()
	command, _, err := root.Find(args)
	if err != nil {
		t.Fatal(err)
	}
	return command
}
