package main

import (
	"context"
	"errors"
	"io"
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

func TestInitPrerequisitesRequirePackAndEveryRole(t *testing.T) {
	clone := t.TempDir()
	roles := []string{"triage", "spec", "implement", "review", "planning"}
	makePack := func(t *testing.T) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(clone, "pack", "roles"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, role := range roles {
			if err := os.WriteFile(filepath.Join(clone, "pack", "roles", role+".md"), []byte(role), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	prerequisites := initPrerequisites{
		lookPath: func(name string) (string, error) { return "/" + name, nil },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/git" {
				return []byte(clone), nil
			}
			return nil, nil
		},
		stat: os.Stat,
	}
	answers := initAnswers{RepositoryName: "app", RepositoryURL: "https://github.com/example/app", ClonePath: clone}

	err := checkInitPrerequisites(t.Context(), prerequisites, answers)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(clone, "pack")) {
		t.Fatalf("missing pack error=%v", err)
	}

	makePack(t)
	missingRole := filepath.Join(clone, "pack", "roles", "review.md")
	if err = os.Remove(missingRole); err != nil {
		t.Fatal(err)
	}
	err = checkInitPrerequisites(t.Context(), prerequisites, answers)
	if err == nil || !strings.Contains(err.Error(), missingRole) {
		t.Fatalf("missing role error=%v", err)
	}

	if err = os.WriteFile(missingRole, []byte("review"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = checkInitPrerequisites(t.Context(), prerequisites, answers); err != nil {
		t.Fatalf("complete pack: %v", err)
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
	if err := os.MkdirAll(filepath.Join(clone, "pack", "roles"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"triage", "spec", "implement", "review", "planning"} {
		if err := os.WriteFile(filepath.Join(clone, "pack", "roles", role+".md"), []byte(role), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main", ClonePath: clone}
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
	if loaded.Workspace != "demo" || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "app" || loaded.DefaultSetup != "default" || loaded.PackDir != filepath.Join(clone, "pack") {
		t.Fatalf("config=%+v", loaded)
	}
	t.Chdir(t.TempDir())
	if _, err = pack.Load(loaded.PackDir); err != nil {
		t.Fatalf("load generated absolute pack path from non-clone cwd: %v", err)
	}
}

func TestInitAPIKeyFollowsEmittedInProcessRouting(t *testing.T) {
	candidate, err := defaultInitConfig("postgres://example", initAnswers{ClonePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err = checkInitAPIKey(candidate, ""); err == nil || !strings.Contains(err.Error(), "CONVEYOR_API_KEY") {
		t.Fatalf("missing key error=%v", err)
	}
	if err = checkInitAPIKey(candidate, "provider-key"); err != nil {
		t.Fatalf("present key: %v", err)
	}

	allMCP := config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Execution: config.ExecutionMCP},
		"spec":   {Execution: config.ExecutionMCP},
		"review": {Execution: config.ExecutionMCP},
	}}}
	if err = checkInitAPIKey(allMCP, ""); err != nil {
		t.Fatalf("config without in-process routing: %v", err)
	}
}

func TestInitializeDeploymentRejectsMissingAPIKeyBeforeWritingConfig(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
	t.Setenv("CONVEYOR_API_TOKEN", "operator-token")
	t.Setenv("CONVEYOR_API_KEY", "")
	configPath := filepath.Join(t.TempDir(), "nested", "conveyor.yaml")
	err := initializeDeployment(t.Context(), io.Discard, configPath, initAnswers{ClonePath: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "CONVEYOR_API_KEY") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config was written before API-key validation: %v", statErr)
	}
}
