package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

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
