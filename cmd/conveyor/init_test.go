package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
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
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://init@db.invalid/conveyor")
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "trunk"}
	candidate, err := defaultInitConfig("postgres://init@db.invalid/conveyor", answers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := config.MarshalDeployment(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	assertDatabaseNodeIsBackendOnly(t, data, "postgres")
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
	if len(loaded.Review.Seats) != 1 || loaded.MaxBounces != 10 || loaded.Review.Seats[0] != (config.ReviewSeat{}) {
		t.Fatalf("generated policy = seats %+v bounces %d", loaded.Review.Seats, loaded.MaxBounces)
	}
	if loaded.Database.URL != "postgres://init@db.invalid/conveyor" || loaded.Workspace != "demo" || loaded.Repos[0].Base != "trunk" || loaded.Repos[0].GitHub != "example/app" {
		t.Fatalf("deployment identity = database %+v workspace %q repos %+v", loaded.Database, loaded.Workspace, loaded.Repos)
	}
}

// legacyExecutorInitConfig renders the configuration that init wrote before
// DEC-56, with a harness, a default setup, and executor models.
func legacyExecutorInitConfig(t *testing.T, databaseURL string, answers initAnswers) []byte {
	t.Helper()
	const harness = "codex"
	settings := config.ContextualExecutionSettings{
		ControlPlane: config.ControlPlaneSettings{
			Triage:   config.ModelTimeoutSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
			Planning: config.PlanningSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
		},
		Spec:           config.ImplementationSettings{Harness: harness, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "30m"},
		Implementation: config.ImplementationSettings{Harness: harness, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "4h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h"},
	}
	review := config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-5.6-terra", Harness: harness, Effort: "high"}}}
	data, err := yaml.Marshal(config.Config{
		Workspace: answers.WorkspaceID, MaxBounces: 10,
		WorkOrderQueueTimeoutText: config.DefaultWorkOrderQueueTimeoutText,
		Database:                  config.DatabaseForURL(databaseURL),
		ExecutionSettings:         &settings,
		Review:                    review,
		Execution:                 config.ExecutionPolicy{SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
		Repos:                     []config.Repo{{Name: answers.RepositoryName, URL: answers.RepositoryURL, GitHub: initGitHubSlug(answers.RepositoryURL), Base: answers.BaseBranch, InstallConveyor: config.InstallSwitch(true)}},
		Monitor:                   config.MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The retired harness registry and setups stay authored YAML only.
	var root map[string]any
	if err = yaml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	asYAML := func(value any) any {
		encoded, marshalErr := yaml.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var decoded any
		if marshalErr = yaml.Unmarshal(encoded, &decoded); marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return decoded
	}
	setting, panel := asYAML(settings), asYAML(review)
	root["harnesses"] = []any{map[string]any{"name": harness, "mcp_transport": "toml_override", "command": []any{"codex", "exec", "{prompt}", "{mcp_config}"}, "probe_command": []any{"codex", "--version"}, "probe_timeout": "10s"}}
	root["setups"] = []any{map[string]any{"name": "default", "execution_settings": setting, "review": panel, "refresh_review": config.RefreshReviewDelta}}
	root["default_setup"] = "default"
	if data, err = yaml.Marshal(root); err != nil {
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
	if _, loadErr := config.LoadClient(path, nil); loadErr != nil {
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

// The generated file names the backend the URL selects and holds no URL, and
// the loader restores the URL from the environment (req-security-boundaries REQ-2).
func TestDefaultInitConfigSelectsSingleStore(t *testing.T) {
	const password = "fixture-singlestore-pw-41d"
	for _, raw := range []string{"singlestore://root:" + password + "@localhost/conveyor_test", "mysql://root:" + password + "@localhost/conveyor_test", "root:" + password + "@tcp(localhost:3306)/conveyor_test"} {
		cfg, err := defaultInitConfig(raw, initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"})
		if err != nil || cfg.Database.Backend != "singlestore" || cfg.Database.URL != "" {
			t.Fatalf("init database backend=%q holds URL=%t err=%v", cfg.Database.Backend, cfg.Database.URL != "", err)
		}
		data, err := config.MarshalDeployment(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertDatabaseNodeIsBackendOnly(t, data, "singlestore")
		if strings.Contains(string(data), password) {
			t.Fatal("generated file holds the password")
		}
		t.Setenv("CONVEYOR_DATABASE_URL", raw)
		loaded, err := config.ParseDeployment(data, "generated.yaml", nil)
		if err != nil {
			t.Fatalf("generated file does not load: %v", err)
		}
		if loaded.Database.Backend != "singlestore" || loaded.Database.URL != raw {
			t.Fatalf("loaded backend=%q restored URL=%t", loaded.Database.Backend, loaded.Database.URL == raw)
		}
	}
}

// Fixture values are fake and keep the real shapes. A failure message names
// which value appeared and never repeats the text that held it.
const (
	initFixturePassword = "fixture-db-pw-5e1c"
	initFixtureUser     = "fixture-db-user"
	initFixtureHost     = "fixture-db-host.invalid"
	initFixtureDatabase = "fixture_db_name"
	initFixtureURL      = "postgres://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	initFixtureToken    = "fixture-operator-token-9b2"

	initProcessHelperEnv     = "CONVEYOR_INIT_PROCESS_HELPER"
	initProcessHelperArgsEnv = "CONVEYOR_INIT_PROCESS_HELPER_ARGS"
)

func initFixtureAnswers() initAnswers {
	return initAnswers{
		Organization: "Fixture Org", OperatorName: "Fixture Operator", OperatorEmail: "owner@example.test",
		WorkspaceID: "demo", WorkspaceName: "Demo", RepositoryName: "app",
		RepositoryURL: "https://github.com/example/app", BaseBranch: "main",
	}
}

// assertNoInitSecret fails when text holds the database password, the URL, or
// any URL part, or the operator token. It reports the label and the part only.
func assertNoInitSecret(t *testing.T, label, text string) {
	t.Helper()
	for part, value := range map[string]string{
		"password": initFixturePassword, "URL": initFixtureURL, "user": initFixtureUser,
		"host": initFixtureHost, "database name": initFixtureDatabase, "operator token": initFixtureToken,
	} {
		if strings.Contains(text, value) {
			t.Errorf("%s holds the %s", label, part)
		}
	}
}

// assertDatabaseNodeIsBackendOnly requires the file's database node to name the
// backend and nothing else. It reports key counts, never values.
func assertDatabaseNodeIsBackendOnly(t *testing.T, data []byte, backend string) {
	t.Helper()
	var document struct {
		Database map[string]any `yaml:"database"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal("generated file is not YAML")
	}
	if _, holdsURL := document.Database["url"]; holdsURL {
		t.Fatal("generated file has a database.url key")
	}
	if len(document.Database) != 1 || document.Database["backend"] != backend {
		t.Fatalf("database node has %d keys, want only backend %q", len(document.Database), backend)
	}
}

// initFixtureStore shares one volatile store across init runs, and counts the
// sign-in links issued. Each hook replaces one store method with a failure.
type initFixtureStore struct {
	store.Backend
	linksIssued      int
	failBootstrap    error
	failVerify       error
	failCreate       error
	failGetWorkspace error
	failIssue        error
}

func (s *initFixtureStore) Close() {}

func (s *initFixtureStore) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, token string) (bool, error) {
	if s.failBootstrap != nil {
		return false, s.failBootstrap
	}
	return s.Backend.BootstrapIdentity(ctx, identity, token)
}

func (s *initFixtureStore) VerifyPersonalAccessToken(ctx context.Context, token string) (core.IdentityUser, error) {
	if s.failVerify != nil {
		return core.IdentityUser{}, s.failVerify
	}
	return s.Backend.VerifyPersonalAccessToken(ctx, token)
}

func (s *initFixtureStore) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	if s.failCreate != nil {
		return core.Workspace{}, s.failCreate
	}
	return s.Backend.CreateWorkspace(ctx, id, name, cfg)
}

func (s *initFixtureStore) GetWorkspace(ctx context.Context, id string) (core.Workspace, error) {
	if s.failGetWorkspace != nil {
		return core.Workspace{}, s.failGetWorkspace
	}
	return s.Backend.GetWorkspace(ctx, id)
}

func (s *initFixtureStore) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	if s.failIssue != nil {
		return core.IssuedSignInLink{}, s.failIssue
	}
	s.linksIssued++
	return s.Backend.IssueSignInLink(ctx, email)
}

// useInitFixtureStore sets the environment init requires and opens the volatile
// store in place of the backend the URL selects. observe, when set, runs at the
// open, the moment a new configuration is staged on disk and not yet published.
func useInitFixtureStore(t *testing.T, observe func()) *initFixtureStore {
	t.Helper()
	t.Setenv("CONVEYOR_DATABASE_URL", initFixtureURL)
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	t.Setenv(config.PublicURLEnv, "https://conveyor.example/")
	fixture := &initFixtureStore{Backend: store.NewVolatileBackend()}
	previous := initOpenStore
	initOpenStore = func(context.Context, config.Database) (store.Backend, error) {
		if observe != nil {
			observe()
		}
		return fixture, nil
	}
	t.Cleanup(func() { initOpenStore = previous })
	return fixture
}

// captureInitLog returns the standard logger's output, where init writes its
// warnings, so a test can check that stream too.
func captureInitLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buffer
}

// assertOnlyDirectoryEntries proves a run left no staged temporary file.
func assertOnlyDirectoryEntries(t *testing.T, directory string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("directory holds %v, want %v", names, want)
	}
}

// The success path: the staged file, the published file, the "Next" output, and
// the stored workspace document hold no secret, and a rerun with the same or a
// padded environment value passes and changes nothing (req-security-boundaries
// REQ-2; component-identity-membership invariant 14).
func TestInitGeneratedConfigHoldsNoDatabaseURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, backend string }{
		{"postgres URL", initFixtureURL, "postgres"},
		{"postgres keyword string", "host=" + initFixtureHost + " user=" + initFixtureUser + " password=" + initFixturePassword + " dbname=" + initFixtureDatabase, "postgres"},
		{"singlestore URL", "singlestore://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase, "singlestore"},
		{"mysql URL", "mysql://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase, "singlestore"},
		{"mysql DSN", initFixtureUser + ":" + initFixturePassword + "@tcp(" + initFixtureHost + ":3306)/" + initFixtureDatabase, "singlestore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			stagedFiles := 0
			fixture := useInitFixtureStore(t, func() {
				entries, err := os.ReadDir(directory)
				if err != nil {
					t.Error(err)
					return
				}
				for _, entry := range entries {
					if !strings.HasPrefix(entry.Name(), ".conveyor-init-") {
						continue
					}
					stagedFiles++
					staged, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
					if readErr != nil {
						t.Error(readErr)
						return
					}
					assertNoInitSecret(t, "staged temporary file", string(staged))
					assertDatabaseNodeIsBackendOnly(t, staged, tc.backend)
					info, statErr := entry.Info()
					if statErr != nil || info.Mode().Perm() != 0o600 {
						t.Errorf("staged file mode=%v err=%v, want 0600", info.Mode().Perm(), statErr)
					}
				}
			})
			t.Setenv("CONVEYOR_DATABASE_URL", tc.raw)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, path, initFixtureAnswers()); err != nil {
				t.Fatalf("init failed: %v", err)
			}
			if stagedFiles != 1 {
				t.Fatalf("observed %d staged files, want 1", stagedFiles)
			}
			published, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertNoInitSecret(t, "published file", string(published))
			assertDatabaseNodeIsBackendOnly(t, published, tc.backend)
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
			if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("published file stat err=%v", statErr)
			}
			first := output.String()
			assertNoInitSecret(t, "init output", first)
			assertNoInitSecret(t, "init log", logged.String())
			for _, required := range []string{"Next: run `conveyord install --config " + path + "`", "CONVEYOR_DATABASE_URL", filepath.Join(directory, ".env"), "--env-file", "CONVEYOR_ENV_FILE"} {
				if !strings.Contains(first, required) {
					t.Errorf("init output lacks %q", required)
				}
			}
			if strings.Contains(first, "Notice:") {
				t.Error("a generated file drew the legacy URL notice")
			}
			if signInTokenFromOutput(t, first) == "" || fixture.linksIssued != 1 {
				t.Fatalf("first run issued %d links", fixture.linksIssued)
			}
			// conveyord loads this file: the loader takes the URL from the
			// environment and keeps the backend the file names.
			loaded, err := config.LoadDeployment(path, nil)
			if err != nil {
				t.Fatal("generated file does not load")
			}
			if loaded.Database.Backend != tc.backend || loaded.Database.URL != tc.raw {
				t.Fatalf("loaded backend=%q restored URL=%t", loaded.Database.Backend, loaded.Database.URL == tc.raw)
			}
			// The stored workspace document holds no secret either.
			document, err := fixture.WorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"))
			if err != nil {
				t.Fatalf("read stored workspace document: %v", err)
			}
			stored, err := yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			assertNoInitSecret(t, "stored workspace document", string(stored))

			for _, rerun := range []struct{ name, value string }{{"same value", tc.raw}, {"padded value", "  " + tc.raw + " \n"}} {
				t.Setenv("CONVEYOR_DATABASE_URL", rerun.value)
				output.Reset()
				if err = initializeDeployment(t.Context(), &output, path, initFixtureAnswers()); err != nil {
					t.Fatalf("%s rerun failed: %v", rerun.name, err)
				}
				assertNoInitSecret(t, rerun.name+" rerun output", output.String())
				assertNoInitSecret(t, rerun.name+" rerun log", logged.String())
				if !strings.Contains(output.String(), "already initialized; issuing a fresh") || signInTokenFromOutput(t, output.String()) == "" {
					t.Fatalf("%s rerun did not report the completed deployment and a fresh link", rerun.name)
				}
				if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, published) {
					t.Fatalf("%s rerun changed the file (read err %v)", rerun.name, readErr)
				}
			}
		})
	}
}

// A file an earlier init wrote still reruns, stays byte-equal, and draws a notice
// that says it holds a URL and prints no part of it.
func TestInitRerunOverLegacyURLFile(t *testing.T) {
	answers := initFixtureAnswers()
	base := legacyExecutorInitConfig(t, initFixtureURL, answers)
	withoutBackend := bytes.Replace(base, []byte("backend: postgres\n"), nil, 1)
	if bytes.Equal(base, withoutBackend) {
		t.Fatal("fixture has no explicit backend to remove")
	}
	for _, tc := range []struct {
		name  string
		file  []byte
		value string
	}{
		{"same URL", base, initFixtureURL},
		{"padded environment value", base, "\t" + initFixtureURL + "  \n"},
		{"backend inferred from the file's URL", withoutBackend, initFixtureURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			if err := os.WriteFile(path, tc.file, 0o600); err != nil {
				t.Fatal(err)
			}
			fixture := useInitFixtureStore(t, nil)
			t.Setenv("CONVEYOR_DATABASE_URL", tc.value)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, path, answers); err != nil {
				t.Fatalf("legacy rerun failed: %v", err)
			}
			text := output.String()
			assertNoInitSecret(t, "legacy rerun output", text)
			assertNoInitSecret(t, "legacy rerun log", logged.String())
			if !strings.Contains(text, "Notice: "+path+" holds a database URL in database.url") {
				t.Error("legacy rerun printed no notice that the file holds a URL")
			}
			if !strings.Contains(text, "Next: run `conveyord install --config "+path+"`.") {
				t.Error("legacy rerun printed no next step")
			}
			if strings.Contains(text, "The config holds no database URL") {
				t.Error("legacy rerun claims the file holds no URL")
			}
			if signInTokenFromOutput(t, text) == "" || fixture.linksIssued != 1 {
				t.Fatalf("legacy rerun issued %d links", fixture.linksIssued)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, tc.file) {
				t.Fatalf("legacy file was rewritten (read err %v)", err)
			}
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
		})
	}
}

// Every refusal after the environment checks, with an existing file or a
// failing store: nonzero result, no secret in the error, output, or log, no
// staged file left behind, and an existing file unchanged.
func TestInitRefusalsHoldNoSecret(t *testing.T) {
	answers := initFixtureAnswers()
	legacy := func(databaseURL string, mutate func(*initAnswers)) []byte {
		fileAnswers := answers
		if mutate != nil {
			mutate(&fileAnswers)
		}
		return legacyExecutorInitConfig(t, databaseURL, fileAnswers)
	}
	const otherPassword = "fixture-other-pw-3f8a"
	otherURL := "postgres://" + initFixtureUser + ":" + otherPassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	singleStoreURL := "singlestore://" + initFixtureUser + ":" + otherPassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	// A driver error that repeats the connection string, as an unmasked driver would.
	driverText := func(kind string) error {
		return fmt.Errorf("%s: cannot reach `%s` with password %s", kind, initFixtureURL, initFixturePassword)
	}
	generatedForOtherBackend, err := defaultInitConfig(singleStoreURL, answers)
	if err != nil {
		t.Fatal(err)
	}
	urlLessOtherBackend, err := config.MarshalDeployment(&generatedForOtherBackend)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		existing []byte
		prepare  func(*initFixtureStore)
		want     string
		// published names the entries the directory must hold afterwards.
		published []string
		linksMade int
	}{
		{name: "file states a different URL", existing: legacy(otherURL, nil), want: "its database.url differs from CONVEYOR_DATABASE_URL"},
		{name: "file with no URL names a different backend", existing: urlLessOtherBackend, want: "does not match these initialization answers; refusing"},
		{name: "file with another URL names a different backend", existing: legacy(singleStoreURL, nil), want: "does not match these initialization answers; refusing"},
		{name: "file names a different workspace", existing: legacy(initFixtureURL, func(a *initAnswers) { a.WorkspaceID = "other" }), want: "does not match these initialization answers"},
		{name: "file names a different repository", existing: legacy(initFixtureURL, func(a *initAnswers) { a.RepositoryName = "other" }), want: "does not match these initialization answers"},
		{name: "file with a malformed pool size", existing: []byte("database: {url: \"" + initFixtureURL + "\", max_conns: \"" + initFixtureURL + "\"}\n"), want: "load existing deployment config"},
		{name: "file with an unknown backend", existing: []byte("database: {backend: bogus, url: \"" + initFixtureURL + "\"}\n"), want: "load existing deployment config"},
		{name: "file that is not YAML", existing: []byte("database: {url: \"" + initFixtureURL + "\"\n"), want: "load existing deployment config"},
		{name: "bootstrap failure", prepare: func(s *initFixtureStore) { s.failBootstrap = driverText("bootstrap") }, want: "bootstrap"},
		{name: "operator resolution failure", prepare: func(s *initFixtureStore) { s.failVerify = driverText("verify") }, want: "resolve first operator"},
		{name: "workspace creation failure", prepare: func(s *initFixtureStore) { s.failCreate = driverText("create") }, want: "create first workspace"},
		{name: "workspace read failure after a conflict", prepare: func(s *initFixtureStore) {
			s.failCreate = store.ErrWorkspaceConflict
			s.failGetWorkspace = driverText("read")
		}, want: "inspect existing workspace"},
		{name: "sign-in link failure", prepare: func(s *initFixtureStore) { s.failIssue = driverText("issue") }, want: "safe to retry", published: []string{"conveyor.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			if tc.existing != nil {
				if err := os.WriteFile(path, tc.existing, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fixture := useInitFixtureStore(t, nil)
			if tc.prepare != nil {
				tc.prepare(fixture)
			}
			logged := captureInitLog(t)
			var output strings.Builder
			err := initializeDeployment(t.Context(), &output, path, answers)
			if err == nil {
				t.Fatal("refusal returned no error")
			}
			assertNoInitSecret(t, "error", err.Error())
			if strings.Contains(err.Error(), otherPassword) || strings.Contains(output.String(), otherPassword) || strings.Contains(logged.String(), otherPassword) {
				t.Error("refusal repeats the password the file states")
			}
			assertNoInitSecret(t, "output", output.String())
			assertNoInitSecret(t, "log", logged.String())
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not contain %q", tc.want)
			}
			if fixture.linksIssued != tc.linksMade {
				t.Errorf("refusal issued %d sign-in links", fixture.linksIssued)
			}
			if signInTokenFromOutput(t, output.String()) != "" {
				t.Error("refusal printed a sign-in link")
			}
			switch {
			case tc.existing != nil:
				if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, tc.existing) {
					t.Errorf("existing file changed (read err %v)", readErr)
				}
				assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
			default:
				assertOnlyDirectoryEntries(t, directory, tc.published...)
			}
		})
	}
}

// The real backends refuse without a reachable database. A malformed or
// unreachable connection string reaches the operator through the driver's own
// error text, which must hold no password, and leaves no staged file.
func TestInitDriverFailuresHoldNoPassword(t *testing.T) {
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	const head, tail = "fixture-pw-head-a1", "fixture-pw-tail-b2"
	for _, tc := range []struct{ name, raw string }{
		{"postgres password with a colon in an unparseable URL", "postgres://" + initFixtureUser + ":" + head + ":" + tail + "@" + initFixtureHost + ":notaport/db"},
		{"postgres password with a slash", "postgres://" + initFixtureUser + ":" + head + "/" + tail + "@" + initFixtureHost + "/db"},
		{"postgres malformed keyword string", "host=" + initFixtureHost + " password='" + head + " " + tail + "' broken"},
		{"postgres unreachable", "postgres://" + initFixtureUser + ":" + head + "@127.0.0.1:1/db?connect_timeout=1"},
		{"singlestore unparseable URL", "singlestore://" + initFixtureUser + ":" + head + ":" + tail + "@" + initFixtureHost + ":notaport/db"},
		{"singlestore unreachable", "singlestore://" + initFixtureUser + ":" + head + "@127.0.0.1:1/db"},
		{"mysql malformed DSN", initFixtureUser + ":" + head + "@tcp(127.0.0.1:1/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("CONVEYOR_DATABASE_URL", tc.raw)
			logged := captureInitLog(t)
			var output strings.Builder
			err := initializeDeployment(t.Context(), &output, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
			if err == nil {
				t.Fatal("init reached a store through a broken connection string")
			}
			for _, part := range []string{head, tail} {
				if strings.Contains(err.Error(), part) || strings.Contains(output.String(), part) || strings.Contains(logged.String(), part) {
					t.Error("failure text holds part of the password")
				}
			}
			if strings.Contains(err.Error(), tc.raw) {
				t.Error("failure text holds the connection string")
			}
			assertOnlyDirectoryEntries(t, directory)
		})
	}
}

// A collision created nothing, so it must not look like success: no published
// file, no staged file, no sign-in link, and no "already initialized" line. The
// volatile store applies the same conflict rule as the two durable backends
// (runWorkspaceNameUniqueness); assertInitCollisionsRefused proves it on each.
func TestInitRefusesWorkspaceCollision(t *testing.T) {
	first := initFixtureAnswers()
	for _, tc := range []struct {
		name string
		id   string
		as   string
		want string
	}{
		{"taken name with a new ID", "other", first.WorkspaceName, "is taken by another workspace"},
		{"taken name in another case with a new ID", "other", strings.ToUpper(first.WorkspaceName), "is taken by another workspace"},
		{"same ID with a different name", first.WorkspaceID, "Renamed", "already exists with the name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := useInitFixtureStore(t, nil)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, filepath.Join(t.TempDir(), "conveyor.yaml"), first); err != nil {
				t.Fatalf("first init failed: %v", err)
			}
			issuedBefore := fixture.linksIssued
			answers := first
			answers.WorkspaceID, answers.WorkspaceName = tc.id, tc.as
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			output.Reset()
			err := initializeDeployment(t.Context(), &output, path, answers)
			if err == nil {
				t.Fatal("collision returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("collision error = %v", err)
			}
			assertNoInitSecret(t, "collision error", err.Error())
			assertNoInitSecret(t, "collision output", output.String())
			assertNoInitSecret(t, "collision log", logged.String())
			if output.String() != "" {
				t.Errorf("collision printed %d bytes, want nothing", output.Len())
			}
			if fixture.linksIssued != issuedBefore {
				t.Error("collision issued a sign-in link")
			}
			assertOnlyDirectoryEntries(t, directory)
			workspaces, err := fixture.ListWorkspaces(store.WithActor(t.Context(), store.SystemActor()))
			if err != nil || len(workspaces) != 1 || workspaces[0].ID != first.WorkspaceID || workspaces[0].Name != first.WorkspaceName {
				t.Fatalf("workspaces=%+v err=%v", workspaces, err)
			}
			// The same ID and name stays a safe rerun, even at a new config path.
			output.Reset()
			if err = initializeDeployment(t.Context(), &output, path, first); err != nil {
				t.Fatalf("safe rerun after the refusal failed: %v", err)
			}
			if !strings.Contains(output.String(), "already initialized; issuing a fresh") || signInTokenFromOutput(t, output.String()) == "" {
				t.Fatal("safe rerun did not report the completed deployment and a fresh link")
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatalf("safe rerun published no file: %v", err)
			}
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
		})
	}
}

// A workspace read that fails after a conflict is a failure to decide, never a
// collision answer and never a success.
func TestInitReportsWorkspaceReadFailureAfterConflict(t *testing.T) {
	fixture := useInitFixtureStore(t, nil)
	fixture.failCreate = store.ErrWorkspaceConflict
	fixture.failGetWorkspace = errors.New("connection reset")
	directory := t.TempDir()
	var output strings.Builder
	err := initializeDeployment(t.Context(), &output, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
	if err == nil || !strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "is taken") {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(err, fixture.failGetWorkspace) {
		t.Fatal("scrubbed error lost its cause")
	}
	if fixture.linksIssued != 0 || output.String() != "" {
		t.Fatalf("links=%d output bytes=%d", fixture.linksIssued, output.Len())
	}
	assertOnlyDirectoryEntries(t, directory)
}

func TestDatabaseURLSecretsCoversEachForm(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"postgres://u:plain@h/db", "plain"},
		{"postgres://u:a:b@h:bad/db", "a:b"},
		{"postgres://u:a%40b@h/db", "a@b"},
		{"postgres://u:a%40b@h/db", "a%40b"},
		{"postgres://u@h/db?password=query-pw&sslmode=disable", "query-pw"},
		{"host=h password='quoted pw' dbname=d", "quoted pw"},
		{"host=h password=plain-kw dbname=d", "plain-kw"},
		{"root:dsn-pw@tcp(h:3306)/db?param=a@b", "dsn-pw"},
		{"root:dsn:pw@unix(/tmp/s)/db", "dsn:pw"},
	} {
		found := false
		for _, value := range databaseURLSecrets(tc.raw) {
			found = found || value == tc.want
		}
		if !found {
			t.Errorf("no extracted value equals the expected form %q of a fixture password", tc.want)
		}
	}
	if values := databaseURLSecrets("  "); values != nil {
		t.Fatalf("blank string yields %d values", len(values))
	}
}

// TestInitProcessHelper runs the real CLI entry point in a child process, so a
// test can observe the exit status and both output streams.
func TestInitProcessHelper(t *testing.T) {
	if os.Getenv(initProcessHelperEnv) != "1" {
		t.Skip("child process helper")
	}
	os.Args = append([]string{"conveyor"}, strings.Split(os.Getenv(initProcessHelperArgsEnv), "\x1f")...)
	main()
	os.Exit(0)
}

// runInitProcess runs `conveyor init` in a child process with the parent's
// environment and returns its exit status and output streams.
func runInitProcess(t *testing.T, configPath string, answers initAnswers) (int, string, string) {
	t.Helper()
	args := []string{
		"init", "--config", configPath, "--organization", answers.Organization, "--operator-name", answers.OperatorName,
		"--operator-email", answers.OperatorEmail, "--workspace-id", answers.WorkspaceID, "--workspace-name", answers.WorkspaceName,
		"--repository-name", answers.RepositoryName, "--repository-url", answers.RepositoryURL, "--default-branch", answers.BaseBranch,
	}
	command := exec.Command(os.Args[0], "-test.run=^TestInitProcessHelper$")
	command.Dir = t.TempDir()
	home := t.TempDir()
	command.Env = append(os.Environ(), initProcessHelperEnv+"=1", initProcessHelperArgsEnv+"="+strings.Join(args, "\x1f"), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	status := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("start init process: %v", err)
	}
	return status, stdout.String(), stderr.String()
}

// The installed binary reports a refusal as a nonzero exit with a secret-free
// stderr, here with a connection string no driver accepts.
func TestInitProcessExitsNonzeroWithoutPassword(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://"+initFixtureUser+":fixture-pw-head-a1:fixture-pw-tail-b2@"+initFixtureHost+":notaport/db")
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	directory := t.TempDir()
	status, stdout, stderr := runInitProcess(t, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
	if status != 1 {
		t.Fatalf("exit status = %d, want 1", status)
	}
	for _, part := range []string{"fixture-pw-head-a1", "fixture-pw-tail-b2"} {
		if strings.Contains(stdout, part) || strings.Contains(stderr, part) {
			t.Error("process output holds part of the password")
		}
	}
	if !strings.Contains(stderr, "error: initialize postgres store") {
		t.Errorf("stderr has %d bytes and no store error", len(stderr))
	}
	assertOnlyDirectoryEntries(t, directory)
}
