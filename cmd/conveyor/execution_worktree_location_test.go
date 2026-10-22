package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestWriteWorktreeLocationReplacesLegacyRootAndKeepsAuthoredContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := worktreeLocationTestConfig(t, "")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(original, []byte("\nworktree_root: /srv/old-trees\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = writeWorktreeLocation(path, "demo", config.WorktreeLocation{Mode: config.WorktreeLocationRepository}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "worktree_root") || !strings.Contains(string(data), "worktree_location:\n    mode: repository\n    dir: _worktrees") {
		t.Fatalf("written document:\n%s", data)
	}
	if !strings.Contains(string(data), "# Conveyor deployment/bootstrap config") {
		t.Fatal("authored comments were not preserved")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location.Mode != config.WorktreeLocationRepository || location.Dir != "_worktrees" {
		t.Fatalf("location = %+v, %v", location, ok)
	}
}

func TestWriteWorktreeLocationCreatesAMissingDocument(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "conveyor", "conveyor.yaml")
	if err := writeWorktreeLocation(path, "demo", config.WorktreeLocation{Mode: config.WorktreeLocationMachine}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location.Root != filepath.Join(home, ".conveyor", "worktrees") {
		t.Fatalf("location = %+v, %v", location, ok)
	}
	if len(loaded.Harnesses) == 0 {
		t.Fatal("created document has no harness")
	}
}

func TestConfigSetWorktreeLocationRecordsTheChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := worktreeLocationTestConfig(t, "")
	command := configCmd()
	command.SetOut(io.Discard)
	command.SetArgs([]string{"set", "worktree_location", "machine:~/trees", "--config", path})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location != (config.WorktreeLocation{Mode: config.WorktreeLocationMachine, Root: filepath.Join(home, "trees")}) {
		t.Fatalf("location = %+v, %v", location, ok)
	}
	command = configCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"set", "worktree_location", "home", "--config", path})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "repository, repository:<dir>, machine, or machine:<root>") {
		t.Fatalf("invalid value error = %v", err)
	}
}

func TestExecutionSetupDefaultsRequiresWorktreeLocation(t *testing.T) {
	directory := t.TempDir()
	writeProbeFixture(t, directory, "codex", "codex 1.2.3", true)
	t.Setenv("PATH", directory)
	path := filepath.Join(directory, "conveyor.yaml")
	err := runExecutionSetupDefaults(t.Context(), io.Discard, path, "demo", "")
	if err == nil || !strings.Contains(err.Error(), "--defaults requires --worktree-location") || !strings.Contains(err.Error(), "conveyor config set worktree_location repository --config "+path) {
		t.Fatalf("missing flag error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("--defaults without a location wrote %s: %v", path, statErr)
	}
	if err = runExecutionSetupDefaults(t.Context(), io.Discard, path, "demo", "repository"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location != (config.WorktreeLocation{Mode: config.WorktreeLocationRepository, Dir: "_worktrees"}) {
		t.Fatalf("location = %+v, %v", location, ok)
	}
}

func TestSetupFlowKeepsCustomLocationWhenModeIsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := worktreeLocationTestConfig(t, "  mode: repository\n  dir: .trees\n")
	if choice := initialWorktreeChoice(path, ""); choice != config.WorktreeLocationRepository {
		t.Fatalf("initial choice = %q", choice)
	}
	if err := persistWorktreeChoice(path, "demo", config.WorktreeLocationRepository); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, _ := loaded.ConfiguredWorktreeLocation(); location.Dir != ".trees" {
		t.Fatalf("custom dir lost: %+v", location)
	}
	unconfigured := worktreeLocationTestConfig(t, "")
	if err = persistWorktreeChoice(unconfigured, "demo", ""); err == nil || !strings.Contains(err.Error(), "no worktree location is configured") {
		t.Fatalf("empty choice error = %v", err)
	}
}

func TestLocalExecutionSetupLoaderRequiresWorktreeLocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := worktreeLocationTestConfig(t, "")
	if _, err := loadLocalExecutionSetup(path); err == nil || !strings.Contains(err.Error(), "no worktree location is configured in "+path) {
		t.Fatalf("loader error = %v", err)
	}
	configured := worktreeLocationTestConfig(t, "  mode: repository\n")
	setup, err := loadLocalExecutionSetup(configured)
	if err != nil {
		t.Fatal(err)
	}
	if setup.WorktreeLocation.Mode != config.WorktreeLocationRepository || describeWorktreeLocation(setup.WorktreeLocation) != "repository (_worktrees under each primary checkout)" {
		t.Fatalf("setup location = %+v", setup.WorktreeLocation)
	}
}

func TestForwardedWorktreeContainerFollowsTheChildCheckout(t *testing.T) {
	fixture := newGitFixture(t)
	repository := contextWithWorktreeLocation(t.Context(), config.WorktreeLocation{Mode: config.WorktreeLocationRepository, Dir: "_worktrees"})
	if got, err := forwardedWorktreeContainer(repository, fixture.primary, false); err != nil || got != filepath.Join(fixture.primary, "_worktrees") {
		t.Fatalf("repository container = %q, %v", got, err)
	}
	if got, err := forwardedWorktreeContainer(repository, fixture.primary, true); err != nil || got != "" {
		t.Fatalf("spec container = %q, %v", got, err)
	}
	machine := contextWithWorktreeRoot(t.Context(), "/var/lib/conveyor/worktrees")
	if got, err := forwardedWorktreeContainer(machine, fixture.primary, true); err != nil || got != "/var/lib/conveyor/worktrees" {
		t.Fatalf("machine container = %q, %v", got, err)
	}
	if got, err := forwardedWorktreeContainer(t.Context(), fixture.primary, false); err != nil || got != "" {
		t.Fatalf("unconfigured container = %q, %v", got, err)
	}
}
