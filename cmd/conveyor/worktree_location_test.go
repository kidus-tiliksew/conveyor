package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
	if err = writeWorktreeLocation(path, config.WorktreeLocation{Mode: config.WorktreeLocationRepository}); err != nil {
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
	loaded, err := loadLocalConfig(path)
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
	if err := writeWorktreeLocation(path, config.WorktreeLocation{Mode: config.WorktreeLocationMachine}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadLocalConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location.Root != filepath.Join(home, ".conveyor", "worktrees") {
		t.Fatalf("location = %+v, %v", location, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "worktree_location:\n    mode: machine\n    root: ~/.conveyor/worktrees\n"; string(data) != want {
		t.Fatalf("created document = %q, want only the location %q", data, want)
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
	loaded, err := loadLocalConfig(path)
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

// A location write keeps every other authored key, including execution keys
// the client loader ignores, and the comments beside them
// (component-git-delivery).
func TestWriteWorktreeLocationKeepsIgnoredExecutionKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	original := "# local settings\nharnesses: [] # retired\nkit_permissions: []\nworktree_root: /srv/trees\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeWorktreeLocation(path, config.WorktreeLocation{Mode: config.WorktreeLocationMachine, Root: "/srv/new-trees"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# local settings", "harnesses: [] # retired", "kit_permissions: []", "worktree_location:\n    mode: machine\n    root: /srv/new-trees"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("written document lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "worktree_root") {
		t.Fatalf("legacy worktree_root survived:\n%s", data)
	}
}

// A refused location changes no existing file and creates no missing one.
func TestInvalidWorktreeLocationLeavesFilesUntouched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, value := range []string{"machine:relative", "repository:a/b", "repository:#trees", "repository:!trees", "repository:[trees]", "repository:tr*es", "home"} {
		existing := worktreeLocationTestConfig(t, "  mode: machine\n")
		before, err := os.ReadFile(existing)
		if err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(t.TempDir(), "conveyor.yaml")
		for _, path := range []string{existing, missing} {
			command := configCmd()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs([]string{"set", "worktree_location", value, "--config", path})
			if err := command.Execute(); err == nil {
				t.Fatalf("config set accepted %q for %s", value, path)
			}
		}
		after, err := os.ReadFile(existing)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("%q changed the existing file", value)
		}
		if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
			t.Fatalf("%q created %s: %v", value, missing, statErr)
		}
		entries, err := os.ReadDir(filepath.Dir(existing))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("%q left temporary files beside the config: %v", value, entries)
		}
	}
}

func TestLocalConfigPathPrecedence(t *testing.T) {
	cwd := t.TempDir()
	configRoot := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return configRoot, nil }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })

	userDefault := filepath.Join(configRoot, "conveyor", localConfigName)
	assertResolvedLocalConfig(t, nil, "", userDefault, "user default")
	if err := os.MkdirAll(filepath.Dir(userDefault), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userDefault, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertResolvedLocalConfig(t, nil, "", userDefault, "user default")

	cwdPath := filepath.Join(cwd, localConfigName)
	if err := os.WriteFile(cwdPath, []byte("cwd"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertResolvedLocalConfig(t, nil, "", cwdPath, "working-directory file")

	t.Setenv("CONVEYOR_CONFIG", "env.yaml")
	assertResolvedLocalConfig(t, nil, "", filepath.Join(cwd, "env.yaml"), "environment CONVEYOR_CONFIG")

	command := &cobra.Command{Use: "test"}
	var flagValue string
	command.Flags().StringVar(&flagValue, "config", "", "")
	if err := command.Flags().Set("config", "flag.yaml"); err != nil {
		t.Fatal(err)
	}
	assertResolvedLocalConfig(t, command, flagValue, filepath.Join(cwd, "flag.yaml"), "flag")
}

func TestLocalConfigPathReportsUserConfigError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CONVEYOR_CONFIG", "")
	previousUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return "", errors.New("unavailable") }
	t.Cleanup(func() { userConfigDir = previousUserConfigDir })
	if _, err := resolveLocalConfigPath(nil, ""); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func assertResolvedLocalConfig(t *testing.T, command *cobra.Command, flagValue, wantPath, wantSource string) {
	t.Helper()
	resolved, err := resolveLocalConfigPath(command, flagValue)
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
