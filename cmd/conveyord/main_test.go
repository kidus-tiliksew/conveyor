package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/envfile"
)

func TestWorkspaceQueueRegistrarConvergesOnceAndRetriesFailures(t *testing.T) {
	var calls atomic.Int32
	var logMu sync.Mutex
	var lines []string
	wantErr := errors.New("register unavailable")
	fail := atomic.Bool{}
	fail.Store(true)
	registrar := dispatch.NewWorkspaceQueueRegistrar([]string{"startup"}, func(workspace string) error {
		calls.Add(1)
		if workspace == "retry" && fail.Load() {
			return wantErr
		}
		return nil
	}, func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	if added, err := registrar.Ensure("startup"); err != nil || added {
		t.Fatalf("seeded workspace added=%t err=%v", added, err)
	}
	if added, err := registrar.Ensure("retry"); !errors.Is(err, wantErr) || added {
		t.Fatalf("failed workspace added=%t err=%v", added, err)
	}
	fail.Store(false)
	if added, err := registrar.Ensure("retry"); err != nil || !added {
		t.Fatalf("retried workspace added=%t err=%v", added, err)
	}

	const concurrentCalls = 16
	var wg sync.WaitGroup
	results := make(chan bool, concurrentCalls)
	for range concurrentCalls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			added, err := registrar.Ensure("new")
			if err != nil {
				t.Errorf("concurrent ensure: %v", err)
			}
			results <- added
		}()
	}
	wg.Wait()
	close(results)
	addedCount := 0
	for added := range results {
		if added {
			addedCount++
		}
	}
	if addedCount != 1 {
		t.Fatalf("concurrent additions=%d, want 1", addedCount)
	}
	if added, err := registrar.Ensure("new"); err != nil || added {
		t.Fatalf("second pass added=%t err=%v", added, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("registration calls=%d, want failed retry plus two successes", got)
	}

	logMu.Lock()
	defer logMu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("registration logs=%q, want one per successful new workspace", lines)
	}
	wantNew := "registered queue scheduling for workspace new"
	if lines[1] != wantNew {
		t.Fatalf("new workspace log=%q, want %q", lines[1], wantNew)
	}
}

func TestResolveConveyordListenAddress(t *testing.T) {
	tests := []struct {
		name         string
		flagAddr     string
		flagExplicit bool
		environment  map[string]string
		wantAddr     string
		wantSource   string
		wantError    string
	}{
		{name: "default", flagAddr: "127.0.0.1:8080", wantAddr: "127.0.0.1:8080", wantSource: "default"},
		{name: "PORT", flagAddr: "127.0.0.1:8080", environment: map[string]string{"PORT": "9000"}, wantAddr: "0.0.0.0:9000", wantSource: "PORT"},
		{name: "listen environment", flagAddr: "127.0.0.1:8080", environment: map[string]string{"CONVEYOR_LISTEN_ADDR": "127.0.0.2:7000"}, wantAddr: "127.0.0.2:7000", wantSource: "CONVEYOR_LISTEN_ADDR"},
		{name: "listen environment wins over PORT", flagAddr: "127.0.0.1:8080", environment: map[string]string{"CONVEYOR_LISTEN_ADDR": ":7000", "PORT": "9000"}, wantAddr: ":7000", wantSource: "CONVEYOR_LISTEN_ADDR"},
		{name: "flag wins over environment", flagAddr: "localhost:6000", flagExplicit: true, environment: map[string]string{"CONVEYOR_LISTEN_ADDR": "bad", "PORT": "bad"}, wantAddr: "localhost:6000", wantSource: "flag"},
		{name: "explicit flag equal to default wins", flagAddr: "127.0.0.1:8080", flagExplicit: true, environment: map[string]string{"PORT": "9000"}, wantAddr: "127.0.0.1:8080", wantSource: "flag"},
		{name: "invalid listen environment", flagAddr: "127.0.0.1:8080", environment: map[string]string{"CONVEYOR_LISTEN_ADDR": "localhost", "PORT": "9000"}, wantError: `invalid CONVEYOR_LISTEN_ADDR "localhost"`},
		{name: "non-numeric PORT", flagAddr: "127.0.0.1:8080", environment: map[string]string{"PORT": "http"}, wantError: `invalid PORT "http"`},
		{name: "zero PORT", flagAddr: "127.0.0.1:8080", environment: map[string]string{"PORT": "0"}, wantError: `invalid PORT "0"`},
		{name: "out-of-range PORT", flagAddr: "127.0.0.1:8080", environment: map[string]string{"PORT": "65536"}, wantError: `invalid PORT "65536"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(name string) string { return tt.environment[name] }
			addr, source, err := resolveConveyordListenAddress(tt.flagAddr, tt.flagExplicit, getenv)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error=%v, want substring %q", err, tt.wantError)
				}
				return
			}
			if err != nil || addr != tt.wantAddr || source != tt.wantSource {
				t.Fatalf("addr=%q source=%q err=%v, want addr=%q source=%q", addr, source, err, tt.wantAddr, tt.wantSource)
			}
		})
	}
}

func TestFlagWasSetRecognizesExplicitDefault(t *testing.T) {
	fs := flag.NewFlagSet("conveyord", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	if err := fs.Parse([]string{"-addr", "127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	if *addr != "127.0.0.1:8080" || !flagWasSet(fs, "addr") {
		t.Fatalf("addr=%q explicit=%v, want explicit default", *addr, flagWasSet(fs, "addr"))
	}
}

func TestResolveConveyordShutdownTimeout(t *testing.T) {
	tests := []struct {
		name         string
		flagValue    time.Duration
		flagExplicit bool
		environment  string
		want         time.Duration
		wantSource   string
		wantError    bool
	}{
		{name: "default", flagValue: defaultConveyordShutdownTimeout, want: 25 * time.Second, wantSource: "default"},
		{name: "environment", flagValue: defaultConveyordShutdownTimeout, environment: "8s", want: 8 * time.Second, wantSource: "CONVEYOR_SHUTDOWN_TIMEOUT"},
		{name: "flag wins", flagValue: 12 * time.Second, flagExplicit: true, environment: "8s", want: 12 * time.Second, wantSource: "flag"},
		{name: "invalid environment", flagValue: defaultConveyordShutdownTimeout, environment: "soon", wantError: true},
		{name: "zero environment", flagValue: defaultConveyordShutdownTimeout, environment: "0s", wantError: true},
		{name: "negative flag", flagValue: -time.Second, flagExplicit: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source, err := resolveConveyordShutdownTimeout(tt.flagValue, tt.flagExplicit, func(name string) string {
				if name == "CONVEYOR_SHUTDOWN_TIMEOUT" {
					return tt.environment
				}
				return ""
			})
			if tt.wantError {
				if err == nil {
					t.Fatalf("timeout=%s source=%q, want error", got, source)
				}
				return
			}
			if err != nil || got != tt.want || source != tt.wantSource {
				t.Fatalf("timeout=%s source=%q err=%v, want %s from %s", got, source, err, tt.want, tt.wantSource)
			}
		})
	}
}

func TestResolveConveyordListenAddressFromEnvFile(t *testing.T) {
	tests := []struct {
		name       string
		contents   string
		processEnv map[string]string
		wantAddr   string
		wantSource string
	}{
		{name: "file PORT", contents: "PORT=9000\n", wantAddr: "0.0.0.0:9000", wantSource: "PORT"},
		{name: "file listen address", contents: "CONVEYOR_LISTEN_ADDR=localhost:7000\n", wantAddr: "localhost:7000", wantSource: "CONVEYOR_LISTEN_ADDR"},
		{name: "process PORT remains authoritative", contents: "PORT=9000\n", processEnv: map[string]string{"PORT": "8000"}, wantAddr: "0.0.0.0:8000", wantSource: "PORT"},
		{name: "process listen address remains authoritative", contents: "CONVEYOR_LISTEN_ADDR=localhost:7000\n", processEnv: map[string]string{"CONVEYOR_LISTEN_ADDR": "localhost:8000"}, wantAddr: "localhost:8000", wantSource: "CONVEYOR_LISTEN_ADDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{"CONVEYOR_LISTEN_ADDR", "PORT"} {
				previous, present := os.LookupEnv(name)
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if present {
						_ = os.Setenv(name, previous)
					} else {
						_ = os.Unsetenv(name)
					}
				})
			}
			for name, value := range tt.processEnv {
				if err := os.Setenv(name, value); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := envfile.Load(path); err != nil {
				t.Fatal(err)
			}
			addr, source, err := resolveConveyordListenAddress("127.0.0.1:8080", false, os.Getenv)
			if err != nil || addr != tt.wantAddr || source != tt.wantSource {
				t.Fatalf("addr=%q source=%q err=%v, want addr=%q source=%q", addr, source, err, tt.wantAddr, tt.wantSource)
			}
		})
	}
}

func TestLoadConveyordPackUsesEmbeddedDefaultAndStrictOverride(t *testing.T) {
	bundle, err := loadConveyordPack(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageTriage)
	if err != nil || strings.TrimSpace(role) == "" {
		t.Fatalf("embedded triage role=%q err=%v", role, err)
	}

	missing := filepath.Join(t.TempDir(), "missing-pack")
	if _, err = loadConveyordPack(&config.Config{PackDir: missing}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing override error=%v, want path %q", err, missing)
	}

	dir := t.TempDir()
	if err = os.MkdirAll(filepath.Join(dir, "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"triage", "planning", "spec", "implement", "verify", "review"} {
		if err = os.WriteFile(filepath.Join(dir, "roles", name+".md"), []byte("override "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err = loadConveyordPack(&config.Config{PackDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	role, err = bundle.Role(core.StageTriage)
	if err != nil || role != "override triage" {
		t.Fatalf("explicit override role=%q err=%v", role, err)
	}
}

func TestLogControlPlaneModelOverrides(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "general")
	t.Setenv(config.TriageModelEnv, "triage")
	t.Setenv(config.PlanningModelEnv, "planning")
	var lines []string
	logControlPlaneModelOverrides(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	want := []string{
		"control-plane model override active: CONVEYOR_CONTROL_PLANE_MODEL=general",
		"control-plane model override active: CONVEYOR_TRIAGE_MODEL=triage",
		"control-plane model override active: CONVEYOR_PLANNING_MODEL=planning",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("startup override logs=%v, want %v", lines, want)
	}
}

func TestResolveConveyordLLMEnvironmentUsesSharedCompatibilityRules(t *testing.T) {
	lines := make([]string, 0, 1)
	warnf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	conflicting := map[string]string{
		config.LLMAPIKeyEnv:            "new-key",
		config.LLMBaseURLEnv:           "https://new.example/v1",
		config.DeprecatedLLMAPIKeyEnv:  "old-key",
		config.DeprecatedLLMBaseURLEnv: "https://old.example/v1",
	}
	environment, err := resolveConveyordLLMEnvironment(func(name string) string { return conflicting[name] }, warnf)
	if err != nil {
		t.Fatal(err)
	}
	if environment.APIKey != "new-key" || environment.BaseURL != "https://new.example/v1" {
		t.Fatalf("environment=%+v", environment)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "takes precedence") {
		t.Fatalf("startup warnings=%q", lines)
	}

	legacy := map[string]string{
		config.DeprecatedLLMAPIKeyEnv:  "old-key",
		config.DeprecatedLLMBaseURLEnv: "https://old.example/v1",
	}
	environment, err = resolveConveyordLLMEnvironment(func(name string) string { return legacy[name] }, warnf)
	if err != nil || environment.APIKey != "old-key" || environment.BaseURL != "https://old.example/v1" {
		t.Fatalf("legacy environment=%+v err=%v", environment, err)
	}
	if len(lines) != 1 {
		t.Fatalf("startup warnings=%q, want exactly one per process", lines)
	}

	if _, err = resolveConveyordLLMEnvironment(func(string) string { return "" }, warnf); err == nil || !strings.Contains(err.Error(), config.LLMAPIKeyEnv) || !strings.Contains(err.Error(), config.DeprecatedLLMAPIKeyEnv) {
		t.Fatalf("missing key error=%v", err)
	}
}

// DEC-59 clause 2; req-delivery-and-forge AC-1.11: conflicting App key
// encryption values are fatal, while a missing or malformed key keeps the
// nonfatal unavailable branch.
func TestResolveConveyordGitHubAppKeySeparatesFatalConflict(t *testing.T) {
	first := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	second := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	conflicting := map[string]string{config.GitHubAppKeyEncryptionKeyEnv: first, config.DeprecatedGitHubAppKeyEncryptionKeyEnv: second}
	key, err := resolveConveyordGitHubAppKey(func(name string) string { return conflicting[name] }, logf)
	if !errors.Is(err, config.ErrGitHubAppKeyEncryptionKeyConflict) || key != nil {
		t.Fatalf("conflict key=%x err=%v", key, err)
	}
	if !strings.Contains(err.Error(), config.GitHubAppKeyEncryptionKeyEnv) || !strings.Contains(err.Error(), config.DeprecatedGitHubAppKeyEncryptionKeyEnv) || strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
		t.Fatalf("conflict error=%q", err)
	}
	if len(lines) != 0 {
		t.Fatalf("conflict fell through to the warning branch: %q", lines)
	}

	for name, environment := range map[string]map[string]string{
		"missing":   {},
		"malformed": {config.GitHubAppKeyEncryptionKeyEnv: "not-base64"},
	} {
		lines = nil
		key, err = resolveConveyordGitHubAppKey(func(variable string) string { return environment[variable] }, logf)
		if err != nil || key != nil {
			t.Fatalf("%s key=%x err=%v, want nonfatal absence", name, key, err)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "GitHub App key encryption unavailable until configured") || !strings.Contains(lines[0], config.GitHubAppKeyEncryptionKeyEnv) {
			t.Fatalf("%s log=%q", name, lines)
		}
	}

	lines = nil
	canonical := map[string]string{config.GitHubAppKeyEncryptionKeyEnv: first, config.DeprecatedGitHubAppKeyEncryptionKeyEnv: first}
	key, err = resolveConveyordGitHubAppKey(func(name string) string { return canonical[name] }, logf)
	if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{3}, 32)) || len(lines) != 0 {
		t.Fatalf("equal values key=%x err=%v log=%q", key, err, lines)
	}
}

// The real daemon entry point refuses to start on conflicting App key
// encryption values before it opens a store or bootstraps identity, and its
// log names both variables without either value.
func TestConveyordRefusesConflictingGitHubAppKeys(t *testing.T) {
	if path := os.Getenv("CONVEYOR_APP_KEY_CONFLICT_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0"}
		main()
		t.Fatal("conveyord started with conflicting App key encryption values")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	envPath := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(envPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`workspace: startup
database: {backend: postgres}
routing:
  stages:
    triage: {model: fixture, timeout: 20m, execution: in_process}
    spec: {model: fixture, timeout: 30m, execution: mcp}
    implement: {model: fixture, timeout: 4h, execution: mcp}
    review: {model: fixture, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`), 0600); err != nil {
		t.Fatal(err)
	}
	first := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	second := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32))
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConveyordRefusesConflictingGitHubAppKeys$")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CONVEYOR_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env,
		"CONVEYOR_APP_KEY_CONFLICT_CONFIG="+cfgPath,
		"CONVEYOR_ENV_FILE="+envPath,
		// An unreachable database proves the refusal precedes store open.
		"CONVEYOR_DATABASE_URL=postgres://conveyor@127.0.0.1:1/unreachable?connect_timeout=1",
		"CONVEYOR_API_TOKEN=startup-fixture-token",
		"CONVEYOR_LLM_API_KEY=unused-fixture-key",
		config.GitHubAppKeyEncryptionKeyEnv+"="+first,
		config.DeprecatedGitHubAppKeyEncryptionKeyEnv+"="+second,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("conveyord exit err=%v output=%s", err, output)
	}
	startupLog := string(output)
	if !strings.Contains(startupLog, "conflicting GitHub App key encryption keys") || !strings.Contains(startupLog, config.GitHubAppKeyEncryptionKeyEnv) || !strings.Contains(startupLog, config.DeprecatedGitHubAppKeyEncryptionKeyEnv) {
		t.Fatalf("startup log lacks the conflict: %s", startupLog)
	}
	if strings.Contains(startupLog, first) || strings.Contains(startupLog, second) {
		t.Fatalf("startup log echoes a key value: %s", startupLog)
	}
	if strings.Contains(startupLog, "open store") || strings.Contains(startupLog, "using durable") || strings.Contains(startupLog, "unavailable until configured") || strings.Contains(startupLog, "bootstrap") {
		t.Fatalf("conflict did not stop startup before the store: %s", startupLog)
	}
}
