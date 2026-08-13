package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"gopkg.in/yaml.v3"
)

func TestInitPrerequisitesNameEveryMissingRequirement(t *testing.T) {
	clone := t.TempDir()
	answers := initAnswers{RepositoryName: "app", RepositoryURL: "github.com/example/app", ClonePath: clone}
	tests := []struct {
		name          string
		prerequisites initPrerequisites
		want          string
	}{
		{name: "git absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "", errors.New(name + " absent") }}, want: "git is required"},
		{name: "gh absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) {
			if name == "git" {
				return "/git", nil
			}
			return "", errors.New("gh absent")
		}}, want: "run `gh auth login`"},
		{name: "gh unauthenticated", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "/" + name, nil }, run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/gh" {
				return []byte("no account"), errors.New("exit 1")
			}
			return []byte(clone), nil
		}}, want: "run `gh auth login`"},
		{name: "clone absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "/" + name, nil }, run: func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, nil }, stat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }}, want: "repository clone"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prerequisites.stat == nil {
				test.prerequisites.stat = os.Stat
			}
			if test.prerequisites.run == nil {
				test.prerequisites.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
			}
			err := checkInitPrerequisites(t.Context(), test.prerequisites, answers)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestReadInitAnswersUsesDefaultsAndRequiresRepositoryURL(t *testing.T) {
	t.Setenv(config.OrganizationNameEnv, "Example Org")
	input := strings.NewReader("\nOwner\nowner@example.test\ndemo\nDemo\napp\nhttps://github.com/example/app\nmain\n/tmp/app\n")
	var output strings.Builder
	answers, err := readInitAnswers(input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Organization != "Example Org" || answers.WorkspaceID != "demo" || answers.RepositoryName != "app" || answers.ClonePath != "/tmp/app" {
		t.Fatalf("answers=%+v", answers)
	}
	if !strings.Contains(output.String(), "Organization name [Example Org]") {
		t.Fatalf("prompts=%q", output.String())
	}
}

func TestDefaultInitConfigValidatesWithoutHandEditing(t *testing.T) {
	clone := t.TempDir()
	for _, role := range []string{"triage", "planning", "spec", "implement", "review"} {
		rolePath := filepath.Join(clone, "pack", "roles", role+".md")
		if err := os.MkdirAll(filepath.Dir(rolePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rolePath, []byte(role), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/Example/App.git", BaseBranch: "main", ClonePath: clone}
	candidate, err := defaultInitConfig("postgres://example", answers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "demo" || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "app" || loaded.DefaultSetup != "default" {
		t.Fatalf("config=%+v", loaded)
	}
	resolvedClone, err := resolvedInitClonePath(clone)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PackDir != filepath.Join(resolvedClone, "pack") || loaded.Repos[0].Checkout != resolvedClone || loaded.Repos[0].GitHub != "example/app" {
		t.Fatalf("generated paths and identity=%+v pack=%q", loaded.Repos[0], loaded.PackDir)
	}
	if _, err = pack.Load(loaded.PackDir); err != nil {
		t.Fatalf("load generated config pack from outside clone: %v", err)
	}
}

func TestInitPrerequisitesRequirePackAndConditionalAPIKey(t *testing.T) {
	clone := t.TempDir()
	answers := initAnswers{RepositoryName: "app", RepositoryURL: "https://github.com/example/app", ClonePath: clone}
	prerequisites := initPrerequisites{
		lookPath: func(name string) (string, error) { return "/" + name, nil },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/git" {
				return []byte(clone), nil
			}
			return nil, nil
		},
		stat:   os.Stat,
		getenv: func(string) string { return "" },
	}
	if err := checkInitPrerequisites(t.Context(), prerequisites, answers); err == nil || !strings.Contains(err.Error(), filepath.Join(clone, "pack", "roles", "triage.md")) {
		t.Fatalf("missing pack error=%v", err)
	}
	for _, role := range []string{"triage", "planning", "spec", "implement", "review"} {
		path := filepath.Join(clone, "pack", "roles", role+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(role), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkInitPrerequisites(t.Context(), prerequisites, answers); err == nil || !strings.Contains(err.Error(), "CONVEYOR_API_KEY") {
		t.Fatalf("missing API key error=%v", err)
	}
	prerequisites.getenv = func(name string) string {
		if name == "CONVEYOR_API_KEY" {
			return "present"
		}
		return ""
	}
	if err := checkInitPrerequisites(t.Context(), prerequisites, answers); err != nil {
		t.Fatalf("complete prerequisites: %v", err)
	}
	if configUsesInProcessExecution(config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP}}}}) {
		t.Fatal("API key required for a config with no in-process routes")
	}
}

func TestInitializeDeploymentRejectsMissingAPIKeyBeforeWriting(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
	t.Setenv("CONVEYOR_API_TOKEN", "operator-token")
	t.Setenv("CONVEYOR_API_KEY", "")
	configPath := filepath.Join(t.TempDir(), "nested", "conveyor.yaml")
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main", ClonePath: t.TempDir()}
	err := initializeDeployment(t.Context(), &strings.Builder{}, configPath, answers)
	if err == nil || !strings.Contains(err.Error(), "CONVEYOR_API_KEY") {
		t.Fatalf("missing API key error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(configPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("configuration directory written before API-key failure: %v", statErr)
	}
}
