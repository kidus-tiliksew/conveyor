package config

import (
	"fmt"
	"strings"
)

// PlanningControlPlaneRemedy names the deployment keys an operator sets when
// planning has no control-plane settings, and the restart that applies them
// (component-planning; DEC-56(3)).
const PlanningControlPlaneRemedy = "set execution_settings.control_plane.planning.model, and planning_models for alternate models, in the conveyord deployment file and restart conveyord"

// ParseRuntimeWorkspaceDocument is the one runtime composition every
// backend's RuntimeConfig uses (component-runtime; component-persistence).
// It parses the stored workspace document as policy, exactly as
// ParseStoredWorkspaceDocument does, then composes the deployment's own
// in-process control-plane settings into the returned value: triage and
// planning settings, the planning_models allowlist, and the triage route
// (DEC-56(3)). No stored value replaces them, a policy-only document regains
// no executor detail (req-execution-configuration REQ-7, REQ-8; DEC-56(2)),
// and nothing is written back to the stored document.
func ParseRuntimeWorkspaceDocument(data []byte, deployment *Config, source string) (*Config, error) {
	if deployment == nil {
		return nil, fmt.Errorf("deployment configuration is required")
	}
	cfg, _, err := ParseStoredWorkspaceDocument(data, deployment, source)
	if err != nil {
		return nil, err
	}
	composeDeploymentControlPlane(cfg, deployment)
	return cfg, nil
}

// composeDeploymentControlPlane overlays the deployment's control-plane
// settings onto a parsed workspace value with fresh containers, so a caller's
// mutation or a concurrent composition can change neither the deployment nor
// another result. A deployment without control-plane settings composes none
// and nothing is reconstructed from workspace data.
func composeDeploymentControlPlane(cfg, deployment *Config) {
	if deployment.ExecutionSettings == nil {
		cfg.PlanningModels = nil
		if cfg.ExecutionSettings != nil {
			settings := *cfg.ExecutionSettings
			settings.ControlPlane = ControlPlaneSettings{}
			cfg.ExecutionSettings = &settings
		}
		return
	}
	var settings ContextualExecutionSettings
	if cfg.ExecutionSettings != nil {
		// A legacy stored document keeps its other parsed compatibility
		// fields; only its control-plane settings yield to the deployment.
		settings = *cfg.ExecutionSettings
	} else {
		// A policy-only document carries only policy stage timeouts and
		// execution modes in its routes; derive the same default setup the
		// workspace already resolved, so frozen policy is unchanged.
		settings = *contextualExecutionSettings(cfg.Routing)
	}
	control := deployment.ExecutionSettings.ControlPlane
	settings.ControlPlane = ControlPlaneSettings{Triage: control.Triage, Planning: control.Planning}
	cfg.ExecutionSettings = &settings
	cfg.PlanningModels = cloneStrings(deployment.PlanningModels)
	if route, ok := deployment.Routing.Stages["triage"]; ok && strings.TrimSpace(route.Model) != "" {
		next := make(map[string]StageRoute, len(cfg.Routing.Stages)+1)
		for stage, existing := range cfg.Routing.Stages {
			existing.LegacyHarnesses = cloneStrings(existing.LegacyHarnesses)
			next[stage] = existing
		}
		route.LegacyHarnesses = cloneStrings(route.LegacyHarnesses)
		next["triage"] = route
		cfg.Routing.Stages = next
	}
}
