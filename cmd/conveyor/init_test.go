package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"gopkg.in/yaml.v3"
)

func TestReadInitAnswersUsesDefaultsAndRequiresRepositoryURL(t *testing.T) {
	t.Setenv(config.OrganizationNameEnv, "Example Org")
	input := strings.NewReader("\nOwner\nowner@example.test\ndemo\nDemo\napp\nhttps://github.com/example/app\nmain\n")
	var output strings.Builder
	answers, err := readInitAnswers(input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Organization != "Example Org" || answers.WorkspaceID != "demo" || answers.RepositoryName != "app" {
		t.Fatalf("answers=%+v", answers)
	}
	if !strings.Contains(output.String(), "Organization name [Example Org]") || strings.Contains(strings.ToLower(output.String()), "clone") {
		t.Fatalf("prompts=%q", output.String())
	}
}

func TestDefaultInitConfigValidatesWithoutHandEditing(t *testing.T) {
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/Example/App.git", BaseBranch: "main"}
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
	if loaded.Workspace != "demo" || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "app" || loaded.Repos[0].URL != "https://github.com/Example/App.git" || loaded.DefaultSetup != "default" {
		t.Fatalf("config=%+v", loaded)
	}
	if loaded.PackDir != "" || loaded.Repos[0].Checkout != "" || loaded.Repos[0].GitHub != "example/app" || loaded.Repos[0].Base != "main" {
		t.Fatalf("generated paths and identity=%+v pack=%q", loaded.Repos[0], loaded.PackDir)
	}
	if _, err = pack.Load(loaded.PackDir); err != nil {
		t.Fatalf("load embedded pack for generated config: %v", err)
	}
}

func TestInitPrerequisitesAllowNoPackAndRequireConditionalAPIKey(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	answers := initAnswers{RepositoryName: "app", RepositoryURL: "https://github.com/example/app"}
	prerequisites := initPrerequisites{
		getenv: func(string) string { return "" },
	}
	if err := checkInitPrerequisites(prerequisites, answers); err == nil || !strings.Contains(err.Error(), config.LLMAPIKeyEnv) {
		t.Fatalf("missing API key error=%v", err)
	}
	prerequisites.getenv = func(name string) string {
		if name == config.LLMAPIKeyEnv {
			return "present"
		}
		return ""
	}
	if err := checkInitPrerequisites(prerequisites, answers); err != nil {
		t.Fatalf("prerequisites without host repository tools or clone: %v", err)
	}
	prerequisites.getenv = func(name string) string {
		if name == config.DeprecatedLLMAPIKeyEnv {
			return "legacy-present"
		}
		return ""
	}
	if err := checkInitPrerequisites(prerequisites, answers); err != nil {
		t.Fatalf("legacy API key fallback: %v", err)
	}
	if configUsesInProcessExecution(config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP}}}}) {
		t.Fatal("API key required for a config with no in-process routes")
	}
}

func TestInitializeDeploymentRejectsMissingAPIKeyBeforeWriting(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
	t.Setenv("CONVEYOR_API_TOKEN", "operator-token")
	t.Setenv(config.LLMAPIKeyEnv, "")
	t.Setenv(config.DeprecatedLLMAPIKeyEnv, "")
	configPath := filepath.Join(t.TempDir(), "nested", "conveyor.yaml")
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"}
	err := initializeDeployment(t.Context(), &strings.Builder{}, configPath, answers)
	if err == nil || !strings.Contains(err.Error(), config.LLMAPIKeyEnv) {
		t.Fatalf("missing API key error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(configPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("configuration directory written before API-key failure: %v", statErr)
	}
}
