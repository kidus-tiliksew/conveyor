package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const planHookHelperModeEnv = "PLAN_HOOK_HELPER_MODE"
const planHookHelperMarkerEnv = "PLAN_HOOK_MARKER"

// TestPrePushHookHelperProcess is the hook body when the test binary is invoked
// as the configured command. It only acts when GO_WANT_PLAN_HOOK_HELPER=1.
func TestPrePushHookHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PLAN_HOOK_HELPER") != "1" {
		return
	}
	switch os.Getenv(planHookHelperModeEnv) {
	case "args":
		// Print the draft env var and every argv element, NUL-joined, so the
		// test can assert argv arrives literally and unexpanded.
		os.Stdout.WriteString("draft=" + os.Getenv(planHookDraftEnvVar) + "\n")
		os.Stdout.WriteString("argv=" + strings.Join(os.Args[1:], "\x00") + "\n")
	case "fail":
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "mark":
		marker := os.Getenv(planHookHelperMarkerEnv)
		file, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(4)
		}
		file.WriteString("ran\n")
		file.Close()
	}
	os.Exit(0)
}

func planHookTestBinary(t *testing.T) string {
	t.Helper()
	exe := os.Args[0]
	if !filepath.IsAbs(exe) {
		abs, err := filepath.Abs(exe)
		if err != nil {
			t.Fatalf("resolve test binary: %v", err)
		}
		exe = abs
	}
	return exe
}

func writePlanningConfigFile(t *testing.T, repoRoot, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".conveyor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create .conveyor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "planning.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write planning.yaml: %v", err)
	}
}

func planningConfigYAML(t *testing.T, command []string, timeoutSeconds int, blocking bool) string {
	t.Helper()
	document := map[string]any{
		"schema_version": 1,
		"pre_push_review": map[string]any{
			"command":         command,
			"timeout_seconds": timeoutSeconds,
			"blocking":        blocking,
		},
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal planning config: %v", err)
	}
	return string(data)
}

func hookCommand(binary string) []string {
	return []string{binary, "-test.run=TestPrePushHookHelperProcess", "--"}
}

func TestPlanningConfigAbsentFileSkips(t *testing.T) {
	config, err := loadPlanningConfig(t.TempDir())
	if err != nil {
		t.Fatalf("loadPlanningConfig absent file: %v", err)
	}
	if config.PrePushReview != nil {
		t.Fatalf("absent file must yield nil PrePushReview, got %+v", config.PrePushReview)
	}
}

func TestPlanningConfigParsesDeclaredHook(t *testing.T) {
	repoRoot := t.TempDir()
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, []string{"conveyor-brief-review"}, 900, true))
	config, err := loadPlanningConfig(repoRoot)
	if err != nil {
		t.Fatalf("loadPlanningConfig: %v", err)
	}
	if config.PrePushReview == nil {
		t.Fatal("expected PrePushReview")
	}
	if got := config.PrePushReview.Command; len(got) != 1 || got[0] != "conveyor-brief-review" {
		t.Fatalf("command=%v", got)
	}
	if config.PrePushReview.TimeoutSeconds != 900 || !config.PrePushReview.Blocking {
		t.Fatalf("hook=%+v", config.PrePushReview)
	}
}

func TestPlanningConfigRejectsUnsupportedSchemaVersion(t *testing.T) {
	repoRoot := t.TempDir()
	writePlanningConfigFile(t, repoRoot, "schema_version: 2\npre_push_review:\n  command: [\"hook\"]\n")
	if _, err := loadPlanningConfig(repoRoot); err == nil {
		t.Fatal("expected unsupported schema_version error")
	} else if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlanningConfigRejectsEmptyCommand(t *testing.T) {
	repoRoot := t.TempDir()
	writePlanningConfigFile(t, repoRoot, "schema_version: 1\npre_push_review:\n  command: []\n  blocking: true\n")
	if _, err := loadPlanningConfig(repoRoot); err == nil {
		t.Fatal("expected empty command error")
	} else if !strings.Contains(err.Error(), "command") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlanningConfigRejectsUnknownFields(t *testing.T) {
	repoRoot := t.TempDir()
	writePlanningConfigFile(t, repoRoot, "schema_version: 1\npre_push_review:\n  command: [\"hook\"]\n  unexpected: true\n")
	if _, err := loadPlanningConfig(repoRoot); err == nil {
		t.Fatal("expected strict-parse error for unknown field")
	}
}

func TestPrePushReviewRunsArgvWithoutShell(t *testing.T) {
	repoRoot := t.TempDir()
	draftDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "pwned")
	metacharacters := "; touch " + target + "; $(id) && `whoami`"

	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "args")
	command := append(hookCommand(planHookTestBinary(t)), metacharacters)
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, command, 30, true))

	var stdout, stderr bytes.Buffer
	ran, err := runPrePushReview(context.Background(), repoRoot, draftDir, []string{"sha256:a"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPrePushReview: %v (stderr=%s)", err, stderr.String())
	}
	if !ran {
		t.Fatal("expected hook to run")
	}
	if !strings.Contains(stdout.String(), metacharacters) {
		t.Fatalf("argv not delivered literally; stdout=%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "draft="+draftDir) {
		t.Fatalf("draft dir not exported; stdout=%q", stdout.String())
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("shell metacharacters were evaluated; %s exists", target)
	}
}

func TestPrePushReviewNonBlockingFailureDoesNotError(t *testing.T) {
	repoRoot := t.TempDir()
	draftDir := t.TempDir()
	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "fail")
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, hookCommand(planHookTestBinary(t)), 30, false))

	var stdout, stderr bytes.Buffer
	ran, err := runPrePushReview(context.Background(), repoRoot, draftDir, []string{"sha256:a"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("non-blocking failure must not error: %v", err)
	}
	if !ran {
		t.Fatal("expected hook to run")
	}
	if !strings.Contains(stderr.String(), "non-blocking") {
		t.Fatalf("expected non-blocking warning, stderr=%q", stderr.String())
	}
	if _, statErr := os.Stat(filepath.Join(draftDir, planHookStateFile)); !os.IsNotExist(statErr) {
		t.Fatal("failed hook must not record a pass")
	}
}

func TestPrePushReviewBlockingFailureErrors(t *testing.T) {
	repoRoot := t.TempDir()
	draftDir := t.TempDir()
	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "fail")
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, hookCommand(planHookTestBinary(t)), 30, true))

	var stdout, stderr bytes.Buffer
	ran, err := runPrePushReview(context.Background(), repoRoot, draftDir, []string{"sha256:a"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected blocking failure to error")
	}
	if !ran {
		t.Fatal("expected hook to run")
	}
	if _, statErr := os.Stat(filepath.Join(draftDir, planHookStateFile)); !os.IsNotExist(statErr) {
		t.Fatal("failed hook must not record a pass")
	}
}

func TestPrePushReviewTimeoutEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("timeout test")
	}
	repoRoot := t.TempDir()
	draftDir := t.TempDir()
	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "sleep")
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, hookCommand(planHookTestBinary(t)), 1, true))

	start := time.Now()
	var stdout, stderr bytes.Buffer
	ran, err := runPrePushReview(context.Background(), repoRoot, draftDir, []string{"sha256:a"}, &stdout, &stderr)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !ran {
		t.Fatal("expected hook to run")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("timeout not enforced; elapsed=%s", elapsed)
	}
}

func TestPrePushReviewSkipsUnchangedAndRerunsOnChange(t *testing.T) {
	repoRoot := t.TempDir()
	draftDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "runs")
	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "mark")
	t.Setenv(planHookHelperMarkerEnv, marker)
	writePlanningConfigFile(t, repoRoot, planningConfigYAML(t, hookCommand(planHookTestBinary(t)), 30, true))

	markerLines := func() int {
		t.Helper()
		data, err := os.ReadFile(marker)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatalf("read marker: %v", err)
		}
		return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
	}

	hashes := []string{"sha256:a", "sha256:b"}
	ran, err := runPrePushReview(context.Background(), repoRoot, draftDir, hashes, io.Discard, io.Discard)
	if err != nil || !ran {
		t.Fatalf("first run ran=%v err=%v", ran, err)
	}
	if got := markerLines(); got != 1 {
		t.Fatalf("marker after first run=%d", got)
	}

	ran, err = runPrePushReview(context.Background(), repoRoot, draftDir, hashes, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("second run err=%v", err)
	}
	if ran {
		t.Fatal("unchanged hashes must skip the second run")
	}
	if got := markerLines(); got != 1 {
		t.Fatalf("marker after unchanged run=%d", got)
	}

	ran, err = runPrePushReview(context.Background(), repoRoot, draftDir, []string{"sha256:a", "sha256:c"}, io.Discard, io.Discard)
	if err != nil || !ran {
		t.Fatalf("changed run ran=%v err=%v", ran, err)
	}
	if got := markerLines(); got != 2 {
		t.Fatalf("marker after changed run=%d", got)
	}
}

func TestPrePushReviewAbsentConfigSkips(t *testing.T) {
	ran, err := runPrePushReview(context.Background(), t.TempDir(), t.TempDir(), []string{"sha256:a"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("absent config must skip without error: %v", err)
	}
	if ran {
		t.Fatal("absent config must not run the hook")
	}
}
