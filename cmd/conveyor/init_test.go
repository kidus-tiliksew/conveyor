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
	data, err := config.MarshalDeployment(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadDeployment(path, func(format string, args ...any) {
		t.Fatalf("generated config carried retired execution detail: "+format, args...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "demo" || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "app" || loaded.Repos[0].URL != "https://github.com/Example/App.git" {
		t.Fatalf("config=%+v", loaded)
	}
	if loaded.PackDir != "" || loaded.Repos[0].Checkout != "" || loaded.Repos[0].GitHub != "example/app" || loaded.Repos[0].Base != "main" {
		t.Fatalf("generated paths and identity=%+v pack=%q", loaded.Repos[0], loaded.PackDir)
	}
	if _, err = pack.Load(loaded.PackDir); err != nil {
		t.Fatalf("load embedded pack for generated config: %v", err)
	}
}

// DEC-56(1)-(3); component-identity-membership: the file init writes holds
// policy and the in-process control-plane settings, and no executor key at
// any path.
func TestDefaultInitConfigWritesPolicyOnly(t *testing.T) {
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "trunk"}
	candidate, err := defaultInitConfig("postgres://init@db.invalid/conveyor", answers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := config.MarshalDeployment(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err = yaml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var walk func(string, any)
	walk = func(path string, node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				childPath := strings.TrimPrefix(path+"."+key, ".")
				switch key {
				case "harness", "harnesses", "setups", "default_setup", "routing", "model_policy", "argv", "command",
					"model_args", "effort_args", "probe_command", "mcp_transport", "fallback_model", "fallback_harness",
					"first_activity_timeout", "verify_concurrency", "planning_models":
					t.Fatalf("generated deployment config carries %s:\n%s", childPath, data)
				case "model", "effort", "model_tier":
					if !strings.HasPrefix(childPath, "execution_settings.control_plane.triage.") && !strings.HasPrefix(childPath, "execution_settings.control_plane.planning.") {
						t.Fatalf("generated deployment config carries executor %s:\n%s", childPath, data)
					}
				}
				walk(childPath, child)
			}
		case []any:
			for _, child := range typed {
				walk(path+"[]", child)
			}
		}
	}
	walk("", root)
	loaded, err := config.ParseDeployment(data, "generated.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Execution.SpecApproval || !loaded.Execution.MergeApproval {
		t.Fatalf("generated gates = %+v, want both on", loaded.Execution)
	}
	triage := loaded.Routing.Stages["triage"]
	planning := loaded.ExecutionSettings.ControlPlane.Planning
	if triage.Model != "gpt-5.6-luna" || triage.Effort != "high" || triage.TimeoutText != "20m" || planning.Model != "gpt-5.6-luna" || planning.Effort != "high" {
		t.Fatalf("control-plane settings were not retained: triage=%+v planning=%+v", triage, planning)
	}
	for stage, want := range map[string]string{"spec": "30m", "implement": "4h", "review": "1h"} {
		if got := loaded.Routing.Stages[stage].TimeoutText; got != want {
			t.Fatalf("%s timeout = %q, want %q", stage, got, want)
		}
	}
	if len(loaded.Review.Seats) != 1 || loaded.MaxBounces != 10 || loaded.Harnesses != nil || loaded.Setups != nil {
		t.Fatalf("generated policy = seats %d bounces %d harnesses %v setups %v", len(loaded.Review.Seats), loaded.MaxBounces, loaded.Harnesses, loaded.Setups)
	}
	if loaded.Database.URL != "postgres://init@db.invalid/conveyor" || loaded.Workspace != "demo" || loaded.Repos[0].Base != "trunk" || loaded.Repos[0].GitHub != "example/app" {
		t.Fatalf("deployment identity = database %+v workspace %q repos %+v", loaded.Database, loaded.Workspace, loaded.Repos)
	}
}

// legacyExecutorInitConfig renders the configuration that init wrote before
// DEC-56, with a harness, a default setup, and executor models.
func legacyExecutorInitConfig(t *testing.T, databaseURL string, answers initAnswers) []byte {
	t.Helper()
	harness := config.HarnessTemplates()[0].Harness
	settings := config.ContextualExecutionSettings{
		ControlPlane: config.ControlPlaneSettings{
			Triage:   config.ModelTimeoutSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
			Planning: config.PlanningSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
		},
		Spec:           config.ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "30m"},
		Implementation: config.ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "4h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h"},
	}
	review := config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-5.6-terra", Harness: harness.Name, Effort: "high"}}}
	data, err := yaml.Marshal(config.Config{
		Workspace: answers.WorkspaceID, MaxBounces: 10,
		WorkOrderQueueTimeoutText: config.DefaultWorkOrderQueueTimeoutText,
		Database:                  config.DatabaseForURL(databaseURL),
		ExecutionSettings:         &settings,
		Harnesses:                 []config.Harness{harness},
		Review:                    review,
		Setups:                    []config.ExecutionSetup{{Name: "default", ExecutionSettings: settings, Review: review, RefreshReview: config.RefreshReviewDelta}},
		DefaultSetup:              "default",
		Execution:                 config.ExecutionPolicy{SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
		Repos:                     []config.Repo{{Name: answers.RepositoryName, URL: answers.RepositoryURL, GitHub: initGitHubSlug(answers.RepositoryURL), Base: answers.BaseBranch, InstallConveyor: config.InstallSwitch(true)}},
		Monitor:                   config.MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// An existing file in the pre-DEC-56 init shape loads through the deployment
// loader, so the match check runs instead of a load failure, and the file is
// never rewritten.
func TestInitializeDeploymentLoadsLegacyExecutorConfig(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
	t.Setenv("CONVEYOR_API_TOKEN", "operator-token")
	t.Setenv(config.LLMAPIKeyEnv, "present")
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	legacy := legacyExecutorInitConfig(t, "postgres://example", initAnswers{WorkspaceID: "other", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"})
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	err := initializeDeployment(t.Context(), &strings.Builder{}, path, answers)
	if err == nil || !strings.Contains(err.Error(), "does not match these initialization answers") {
		t.Fatalf("legacy config error = %v, want the mismatch refusal after a successful load", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(legacy) {
		t.Fatalf("legacy config was rewritten: %v", readErr)
	}
	if _, loadErr := config.Load(path); loadErr != nil {
		t.Fatalf("legacy file no longer loads as a client document: %v", loadErr)
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

func TestDefaultInitConfigSelectsSingleStore(t *testing.T) {
	for _, raw := range []string{"singlestore://root@localhost/conveyor_test", "mysql://root@localhost/conveyor_test", "root@tcp(localhost:3306)/conveyor_test"} {
		cfg, err := defaultInitConfig(raw, initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"})
		if err != nil || cfg.Database.Backend != "singlestore" || cfg.Database.URL != raw {
			t.Fatalf("init database=%+v err=%v", cfg.Database, err)
		}
	}
}
