package config

import (
	"errors"
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
// (component-runtime).

// deploymentExecutorStageKeys are removed from execution_settings.spec,
// implementation, and verify.
var deploymentExecutorStageKeys = []string{"harness", "model", "model_policy", "effort"}

// deploymentRetiredRouteKeys are removed from each routing.stages entry. In
// a deployment file the in-process triage stage's model, model tier, and
// effort are not retired: triageRouteAliasKeys moves them to the
// control-plane triage settings instead.
var deploymentRetiredRouteKeys = map[string]bool{
	"model": true, "model_policy": true, "harness": true, "effort": true,
	"execution": true, "harnesses": true, "model_tier": true,
}

// triageRouteAliasKeys are the legacy spellings of the control-plane triage
// settings on routing.stages.triage, each with the setting it supplies. The
// model_tier alias supplies the model only when model is absent, as the
// earlier loader read it.
var triageRouteAliasKeys = []struct{ key, setting string }{
	{"model", "model"}, {"model_tier", "model"}, {"effort", "effort"},
}

// policyStageTimeoutDefaults are the policy stage timeouts that apply when a
// deployment file names none (component-runtime).
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
	stripped, ignored, packDirSet, err := stripExecutionDetail(data, true)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, locatedYAMLError[Config](data, err))
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
// contents, or first_activity_timeout. It never emits database.url, which can
// hold a password: the file names the backend and the loader takes the URL
// from CONVEYOR_DATABASE_URL (req-security-boundaries REQ-2).
func MarshalDeployment(c *Config) ([]byte, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	stripped, _, _, err := stripExecutionDetail(data, true)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	var root map[string]any
	if err := unmarshalYAML(stripped, &root); err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	delete(root, "routing")
	if execution, ok := root["execution"].(map[string]any); ok {
		delete(execution, "first_activity_timeout")
	}
	if database, ok := root["database"].(map[string]any); ok {
		delete(database, "url")
	}
	data, err = yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment config: %w", err)
	}
	return data, nil
}

// DeploymentHoldsDatabaseURL reports whether a deployment document itself
// states database.url. A loaded Config can't say, because the loader fills an
// empty URL from CONVEYOR_DATABASE_URL. It returns no value from the document,
// and its error carries no parser text, so neither can repeat a password.
func DeploymentHoldsDatabaseURL(data []byte) (bool, error) {
	var document struct {
		Database struct {
			URL string `yaml:"url"`
		} `yaml:"database"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return false, errors.New("read database.url from the deployment document")
	}
	return document.Database.URL != "", nil
}

// stripExecutionDetail decodes the document with aliases and merge keys
// resolved, so a removal never touches a shared anchor, and returns it
// without execution detail plus the sorted removed field paths. A deployment
// file (deployment true) keeps its legacy triage route spellings by moving
// them to the control-plane triage settings; a stored workspace row drops
// them, because runtime composition takes triage from the deployment only
// (component-runtime invariants 13 to 15).
func stripExecutionDetail(data []byte, deployment bool) ([]byte, []string, bool, error) {
	var root map[string]any
	if err := unmarshalYAML(data, &root); err != nil {
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
	adapted := false
	if routing, ok := root["routing"].(map[string]any); ok {
		if stages, ok := routing["stages"].(map[string]any); ok {
			for stage, value := range stages {
				mapping, ok := value.(map[string]any)
				if !ok {
					continue
				}
				if stage == "triage" && deployment {
					adapted = adaptLegacyTriageRoute(root, mapping) || adapted
				}
				for key := range mapping {
					if deploymentRetiredRouteKeys[key] {
						drop(mapping, "routing.stages."+retiredRouteStageLabel(stage)+".", key)
					}
				}
			}
		}
	}
	ignored := make([]string, 0, len(removed))
	for name := range removed {
		ignored = append(ignored, name)
	}
	sort.Strings(ignored)
	if len(ignored) == 0 && !adapted {
		return data, nil, packDirSet, nil
	}
	stripped, err := yaml.Marshal(root)
	if err != nil {
		return nil, nil, false, err
	}
	return stripped, ignored, packDirSet, nil
}

// adaptLegacyTriageRoute is the compatibility adapter for a deployment
// file's routing.stages.triage: it moves the route's model (else its
// model_tier alias) and effort to execution_settings.control_plane.triage,
// where the triage route is now read, unless that block already states the
// setting, which keeps the earlier precedence. The keys are removed from the
// route either way and reported as adapted, not ignored, because their values
// still apply (DEC-132(3); component-runtime invariant 13). It reports
// whether it changed the document.
func adaptLegacyTriageRoute(root, route map[string]any) bool {
	present := false
	for _, alias := range triageRouteAliasKeys {
		if _, ok := route[alias.key]; ok {
			present = true
		}
	}
	if !present {
		return false
	}
	triage := controlPlaneTriage(root)
	for _, alias := range triageRouteAliasKeys {
		value, ok := route[alias.key]
		if !ok {
			continue
		}
		delete(route, alias.key)
		if triage != nil && unsetSetting(triage[alias.setting]) && !unsetSetting(value) {
			triage[alias.setting] = value
		}
	}
	return true
}

// controlPlaneTriage returns execution_settings.control_plane.triage,
// creating each absent level. It returns nil when a level holds something
// other than a mapping, which strict decoding then refuses with its own
// error.
func controlPlaneTriage(root map[string]any) map[string]any {
	mapping := root
	for _, key := range []string{"execution_settings", "control_plane", "triage"} {
		value, ok := mapping[key]
		if !ok || value == nil {
			next := map[string]any{}
			mapping[key] = next
			mapping = next
			continue
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		mapping = next
	}
	return mapping
}

// unsetSetting matches how the loader read an absent or blank triage setting.
func unsetSetting(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && text == ""
}

// retiredRouteStageLabel names a stage in the removed-field log line. A stage is
// a free map key, so only a stage this release knows prints by name
// (req-security-boundaries REQ-2; component-runtime).
func retiredRouteStageLabel(stage string) string {
	switch stage {
	case "triage", "spec", "implement", "review", "verify":
		return stage
	}
	return "<unrecognized>"
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
		topLevel, _ := root["review"].(map[string]any)
		distinct, hasDistinct := topLevel["distinct_models"]
		if review, ok := setup["review"]; ok {
			root["review"] = review
		} else {
			delete(root, "review")
		}
		// A setup's review block predates review.distinct_models, so the file's
		// top-level value survives unless the setup states its own (AC-2.9).
		if hasDistinct {
			merged := map[string]any{"distinct_models": distinct}
			if projected, ok := root["review"].(map[string]any); ok {
				for key, value := range projected {
					merged[key] = value
				}
			}
			root["review"] = merged
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
	// A value built in code may already hold a triage route; it applies
	// where the control-plane settings are silent, as the triage stage route
	// did before it became a type of its own.
	if triage.Model == "" {
		triage.Model = c.Routing.Triage.Model
	}
	if triage.Effort == "" {
		triage.Effort = c.Routing.Triage.Effort
	}
	if triage.TimeoutText == "" {
		triage.TimeoutText = c.Routing.Triage.TimeoutText
	}
	for stage, route := range c.Routing.Stages {
		if stage == "triage" {
			// The stripping boundary already moved this route's model and
			// effort to the control-plane triage settings; its timeout
			// still applies here.
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
	if err := resolveDeploymentDatabaseMaxConns(&c.Database, os.LookupEnv); err != nil {
		return nil, err
	}
	triage.Model = strings.TrimSpace(triage.Model)
	triage.Effort = strings.TrimSpace(triage.Effort)
	if triage.Model == "" {
		return nil, fmt.Errorf("execution_settings.control_plane.triage.model is required")
	}
	if triage.Effort != "" && !validResponsesEffort(triage.Effort) {
		return nil, errors.New("execution_settings.control_plane.triage.effort must be minimal, low, medium, or high")
	}
	triageTimeout := DefaultStageTimeout
	if strings.TrimSpace(triage.TimeoutText) == "" {
		triage.TimeoutText = DefaultStageTimeout.String()
	} else if parsed, err := time.ParseDuration(triage.TimeoutText); err != nil || parsed <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.triage.timeout must be a positive duration")
	} else {
		triageTimeout = parsed
	}
	routes := make(map[string]StageRoute, 4)
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
	c.Routing = Routing{Stages: routes, Triage: TriageRoute{
		Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText, Timeout: triageTimeout,
	}}
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
