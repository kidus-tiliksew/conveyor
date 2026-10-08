package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The deployment file is the server configuring itself. It carries pipeline
// policy and the settings of the stages that run inside conveyord, never the
// execution detail that client-local setups own (DEC-56; component-runtime).
// Load stays the client loader for the local execution document
// (component-harness-execution).

// deploymentExecutorStageKeys are removed from execution_settings.spec,
// implementation, and verify.
var deploymentExecutorStageKeys = []string{"harness", "model", "model_policy", "effort"}

// deploymentRetiredRouteKeys are removed from each routing.stages entry. The
// in-process triage stage keeps its model, model tier, and effort.
var deploymentRetiredRouteKeys = map[string]bool{
	"model": true, "model_policy": true, "harness": true, "effort": true,
	"execution": true, "harnesses": true, "model_tier": true,
}

// policyStageTimeoutDefaults are the policy stage timeouts that apply when a
// deployment file names none (component-harness-execution "Defaults").
var policyStageTimeoutDefaults = map[string]string{"spec": "30m", "implement": "4h", "review": "1h", "verify": "1h"}

// LoadDeployment reads the server's deployment file. Execution detail that a
// file written before DEC-56 still carries is removed before validation and
// named through logf, without values. Unknown unrelated keys still fail.
func LoadDeployment(path string, logf func(string, ...any)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseDeployment(data, path, logf)
}

// ParseDeployment is LoadDeployment over bytes already read. path resolves a
// relative pack_dir and names the source in errors and the warning.
func ParseDeployment(data []byte, path string, logf func(string, ...any)) (*Config, error) {
	stripped, ignored, packDirSet, err := stripDeploymentExecutionDetail(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(ignored) != 0 && logf != nil {
		logf("config: %s: ignoring retired execution detail in deployment configuration (DEC-56): %s", path, strings.Join(ignored, ", "))
	}
	var c Config
	if err := decodeKnown(stripped, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.packDirSet = packDirSet
	return normalizeDeployment(&c, path)
}

// MarshalDeployment renders the deployment file that conveyor init writes:
// policy and control-plane settings only. It never emits harnesses, setups,
// routing, a stage harness, model, model policy, or effort, review seat
// contents, or first_activity_timeout.
func MarshalDeployment(c *Config) ([]byte, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	stripped, _, _, err := stripDeploymentExecutionDetail(data)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(stripped, &root); err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	delete(root, "routing")
	if execution, ok := root["execution"].(map[string]any); ok {
		delete(execution, "first_activity_timeout")
	}
	data, err = yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	return data, nil
}

// stripDeploymentExecutionDetail decodes the document with aliases and merge
// keys resolved, so a removal never touches a shared anchor, and returns it
// without execution detail plus the sorted removed field paths.
func stripDeploymentExecutionDetail(data []byte) ([]byte, []string, bool, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, false, err
	}
	if root == nil {
		root = map[string]any{}
	}
	_, packDirSet := root["pack_dir"]
	removed := map[string]struct{}{}
	drop := func(mapping map[string]any, prefix string, keys ...string) {
		for _, key := range keys {
			if _, ok := mapping[key]; ok {
				delete(mapping, key)
				removed[prefix+key] = struct{}{}
			}
		}
	}
	promoteLegacyDefaultSetup(root)
	drop(root, "", "harnesses", "setups", "default_setup")
	if settings, ok := root["execution_settings"].(map[string]any); ok {
		for _, stage := range []string{"spec", "implementation", "verify"} {
			if mapping, ok := settings[stage].(map[string]any); ok {
				drop(mapping, "execution_settings."+stage+".", deploymentExecutorStageKeys...)
			}
		}
		if controlPlane, ok := settings["control_plane"].(map[string]any); ok {
			if mapping, ok := controlPlane["spec"].(map[string]any); ok {
				drop(mapping, "execution_settings.control_plane.spec.", "model", "effort")
			}
		}
		if mapping, ok := settings["review"].(map[string]any); ok {
			drop(mapping, "execution_settings.review.", "execution", "fallback_model", "fallback_harness")
		}
	}
	if review, ok := root["review"].(map[string]any); ok {
		if seats, ok := review["seats"].([]any); ok {
			for _, seat := range seats {
				if mapping, ok := seat.(map[string]any); ok {
					drop(mapping, "review.seats[].", "model", "harness", "effort")
				}
			}
		}
	}
	if routing, ok := root["routing"].(map[string]any); ok {
		if stages, ok := routing["stages"].(map[string]any); ok {
			for stage, value := range stages {
				mapping, ok := value.(map[string]any)
				if !ok {
					continue
				}
				for key := range mapping {
					if !deploymentRetiredRouteKeys[key] {
						continue
					}
					if stage == "triage" && (key == "model" || key == "model_tier" || key == "effort") {
						continue
					}
					drop(mapping, "routing.stages."+stage+".", key)
				}
			}
		}
	}
	ignored := make([]string, 0, len(removed))
	for name := range removed {
		ignored = append(ignored, name)
	}
	sort.Strings(ignored)
	if len(ignored) == 0 {
		return data, nil, packDirSet, nil
	}
	stripped, err := yaml.Marshal(root)
	if err != nil {
		return nil, nil, false, err
	}
	return stripped, ignored, packDirSet, nil
}

// promoteLegacyDefaultSetup makes the setup that default_setup names
// authoritative, as the pre-DEC-56 loader did: its execution_settings and
// review replace the top-level projection, and routing, which that loader
// ignored once setups existed, is removed. Its stage timeouts, review seat
// count, and control-plane settings therefore survive; the executor values
// it carries are stripped afterwards like any other. The setup's
// max_bounces, verify_stage, and refresh_review never reached the workspace
// policy a deployment file seeds, so they stay unread (component-runtime).
func promoteLegacyDefaultSetup(root map[string]any) {
	name, _ := root["default_setup"].(string)
	name = strings.TrimSpace(name)
	setups, _ := root["setups"].([]any)
	if name == "" || len(setups) == 0 {
		return
	}
	for _, entry := range setups {
		setup, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if setupName, _ := setup["name"].(string); strings.TrimSpace(setupName) != name {
			continue
		}
		if settings, ok := setup["execution_settings"]; ok {
			root["execution_settings"] = settings
		} else {
			delete(root, "execution_settings")
		}
		if review, ok := setup["review"]; ok {
			root["review"] = review
		} else {
			delete(root, "review")
		}
		delete(root, "routing")
		return
	}
}

// normalizeDeployment validates a stripped deployment file. It shares the
// scalar and planning rules of the client loader but resolves only the
// in-process triage route and policy stage timeouts, so no executor model,
// harness, or effort is required or retained.
func normalizeDeployment(c *Config, path string) (*Config, error) {
	for _, grant := range c.KitPermissions {
		if err := grant.Validate(); err != nil {
			return nil, err
		}
	}
	if err := validateVerificationToolchains(c.VerificationToolchains); err != nil {
		return nil, err
	}
	var requestedPlanning PlanningSettings
	var triage ModelTimeoutSettings
	timeouts := map[string]string{}
	if settings := c.ExecutionSettings; settings != nil {
		requestedPlanning = settings.ControlPlane.Planning
		triage = settings.ControlPlane.Triage
		timeouts["spec"] = settings.Spec.TimeoutText
		if strings.TrimSpace(timeouts["spec"]) == "" {
			timeouts["spec"] = settings.ControlPlane.Spec.TimeoutText
		}
		timeouts["implement"] = settings.Implementation.TimeoutText
		timeouts["verify"] = settings.Verify.TimeoutText
		timeouts["review"] = settings.Review.TimeoutText
	}
	for stage, route := range c.Routing.Stages {
		if stage == "triage" {
			if triage.Model == "" {
				triage.Model = route.Model
				if triage.Model == "" {
					triage.Model = route.LegacyModelTier
				}
			}
			if triage.Effort == "" {
				triage.Effort = route.Effort
			}
			if triage.TimeoutText == "" {
				triage.TimeoutText = route.TimeoutText
			}
			continue
		}
		if strings.TrimSpace(timeouts[stage]) == "" {
			timeouts[stage] = route.TimeoutText
		}
	}
	if err := normalizeDeploymentScalars(c, path); err != nil {
		return nil, err
	}
	c.Harnesses, c.Setups, c.DefaultSetup = nil, nil, ""
	triage.Model = strings.TrimSpace(triage.Model)
	triage.Effort = strings.TrimSpace(triage.Effort)
	if triage.Model == "" {
		return nil, fmt.Errorf("execution_settings.control_plane.triage.model is required")
	}
	if triage.Effort != "" && !validResponsesEffort(triage.Effort) {
		return nil, fmt.Errorf("execution_settings.control_plane.triage.effort %q must be minimal, low, medium, or high", triage.Effort)
	}
	triageTimeout := DefaultStageTimeout
	if strings.TrimSpace(triage.TimeoutText) == "" {
		triage.TimeoutText = DefaultStageTimeout.String()
	} else if parsed, err := time.ParseDuration(triage.TimeoutText); err != nil || parsed <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.triage.timeout must be a positive duration")
	} else {
		triageTimeout = parsed
	}
	routes := map[string]StageRoute{"triage": {
		Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText, Timeout: triageTimeout,
		Execution: ExecutionInProcess, ModelPolicy: ModelPolicyExplicit,
	}}
	for _, stage := range []string{"spec", "implement", "review", "verify"} {
		timeout := strings.TrimSpace(timeouts[stage])
		if timeout == "" {
			timeout = policyStageTimeoutDefaults[stage]
		}
		parsed, err := time.ParseDuration(timeout)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("execution_settings.%s.timeout must be a positive duration", stageName(stage))
		}
		if c.Execution.FirstActivityTimeout >= parsed {
			return nil, fmt.Errorf("execution.first_activity_timeout must be shorter than %s execution timeout", stageName(stage))
		}
		routes[stage] = StageRoute{Execution: ExecutionMCP, TimeoutText: timeout, Timeout: parsed}
	}
	c.Routing = Routing{Stages: routes}
	if c.Review.Seats == nil {
		c.Review.Seats = []ReviewSeat{{}}
	}
	if len(c.Review.Seats) == 0 {
		return nil, fmt.Errorf("review.seats must contain at least one seat")
	}
	c.Review.Seats = make([]ReviewSeat, len(c.Review.Seats))
	c.ExecutionSettings = nil
	return finishNormalization(c, requestedPlanning)
}
