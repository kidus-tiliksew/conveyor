package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
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
	wantNew := fmt.Sprintf("registered River scheduling for workspace new: queues=%s,%s,%s periodic_job=%s",
		queueargs.DispatchQueue("new"),
		queueargs.ReviewPublicationQueue("new"),
		queueargs.GitHubIssuePublicationQueue("new"),
		queueargs.OrderClockPeriodicID("new"),
	)
	if lines[1] != wantNew {
		t.Fatalf("new workspace log=%q, want %q", lines[1], wantNew)
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
