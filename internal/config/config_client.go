package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// clientExecutionKeys are the top-level keys of an earlier client conveyor.yaml
// that described local execution setups. The CLI keeps no local execution
// setup, so LoadClient removes them before decoding (DEC-104(2);
// component-runtime).
var clientExecutionKeys = []string{"default_setup", "execution", "execution_settings", "harnesses", "planning_models", "review", "routing", "setups"}

// LoadClient is the CLI's loader for conveyor.yaml. It removes every execution
// key, logs one line naming the removed keys (never a value) through logf when
// any were present, decodes the rest strictly, and validates only the
// client-local settings the CLI reads: the worktree location, kit_permissions,
// and verification_toolchains. A removed key is never validated, so a file an
// earlier CLI wrote keeps loading (component-runtime).
func LoadClient(path string, logf func(string, ...any)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stripped, ignored, err := stripClientExecutionKeys(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, locatedYAMLError[Config](data, err))
	}
	if len(ignored) != 0 && logf != nil {
		logf("config: %s: ignoring execution keys %s; Conveyor keeps no local execution setup (DEC-104)", path, strings.Join(ignored, ", "))
	}
	var c Config
	if len(strings.TrimSpace(string(stripped))) != 0 {
		if err := decodeKnown(stripped, &c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	for _, grant := range c.KitPermissions {
		if err := grant.Validate(); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := validateVerificationToolchains(c.VerificationToolchains); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if err := c.normalizeWorktreeLocation(home); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// stripClientExecutionKeys returns the document without the top-level
// execution keys and the sorted names it removed. Aliases and merge keys are
// resolved by the decode, so the returned document is self-contained.
func stripClientExecutionKeys(data []byte) ([]byte, []string, error) {
	var root map[string]any
	if err := unmarshalYAML(data, &root); err != nil {
		return nil, nil, err
	}
	if root == nil {
		return []byte{}, nil, nil
	}
	var ignored []string
	for _, key := range clientExecutionKeys {
		if _, ok := root[key]; ok {
			delete(root, key)
			ignored = append(ignored, key)
		}
	}
	if len(ignored) == 0 {
		return data, nil, nil
	}
	sort.Strings(ignored)
	stripped, err := yaml.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	return stripped, ignored, nil
}
