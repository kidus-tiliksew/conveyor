package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
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
	for _, name := range []string{"triage", "planning", "spec", "implement", "review"} {
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
