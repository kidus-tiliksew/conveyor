package main

import (
	"fmt"
	"slices"
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
