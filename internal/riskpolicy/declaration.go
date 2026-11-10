package riskpolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/governedpath"
	"gopkg.in/yaml.v3"
)

// DeclarationPath is the repository file that declares sensitive paths
// (req-risk-based-review REQ-3).
const DeclarationPath = ".conveyor/risk.yaml"

// DeclarationSchemaVersion is the only schema version a declaration may state.
const DeclarationSchemaVersion = 1

// MaxDeclarationBytes is the declaration size limit, 1 MiB (AC-3.1).
const MaxDeclarationBytes = 1 << 20

// Rule is one declared sensitive-path rule (AC-3.1). MinHumanApprovals is
// optional; nil means the rule states no human approval minimum.
type Rule struct {
	Paths             []string `yaml:"paths" json:"paths"`
	MinTier           Tier     `yaml:"min_tier" json:"min_tier"`
	Description       string   `yaml:"description" json:"description"`
	MinHumanApprovals *int     `yaml:"min_human_approvals,omitempty" json:"min_human_approvals,omitempty"`
}

// Declaration is a parsed .conveyor/risk.yaml.
type Declaration struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	Rules         []Rule `yaml:"rules" json:"rules"`
}

// ParseDeclaration parses declaration bytes under AC-3.1: schema_version 1, a
// closed schema, exactly one YAML document, and a 1 MiB limit. Each rule has a
// non-empty paths list of governed-path globs, a valid min_tier, a non-blank
// description, and an optional min_human_approvals of zero or more.
func ParseDeclaration(data []byte) (*Declaration, error) {
	if len(data) > MaxDeclarationBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", DeclarationPath, MaxDeclarationBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var declaration Declaration
	if err := decoder.Decode(&declaration); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s must contain exactly one YAML document", DeclarationPath)
		}
		return nil, fmt.Errorf("%s: %w", DeclarationPath, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one YAML document", DeclarationPath)
	}
	if err := declaration.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", DeclarationPath, err)
	}
	return &declaration, nil
}

// Validate applies the AC-3.1 rule checks to a declaration value.
func (d *Declaration) Validate() error {
	if d == nil || d.SchemaVersion != DeclarationSchemaVersion {
		return fmt.Errorf("schema_version must be %d", DeclarationSchemaVersion)
	}
	for i, rule := range d.Rules {
		if len(rule.Paths) == 0 {
			return fmt.Errorf("rules[%d].paths must not be empty", i)
		}
		for j, glob := range rule.Paths {
			if err := governedpath.ValidateGlob(glob); err != nil {
				return fmt.Errorf("rules[%d].paths[%d]: %w", i, j, err)
			}
		}
		if !rule.MinTier.Valid() {
			return fmt.Errorf("rules[%d].min_tier must be low, medium, or high", i)
		}
		if strings.TrimSpace(rule.Description) == "" {
			return fmt.Errorf("rules[%d].description must not be blank", i)
		}
		if rule.MinHumanApprovals != nil && *rule.MinHumanApprovals < 0 {
			return fmt.Errorf("rules[%d].min_human_approvals must be zero or more", i)
		}
	}
	return nil
}

// BuiltinRules are the minimums that apply whatever a declaration says: a
// change to a workflow or to the declaration itself needs at least medium and
// one human approval (AC-3.3; component-risk-review invariant 5).
func BuiltinRules() []Rule {
	one := 1
	return []Rule{
		{Paths: []string{".github/workflows/**"}, MinTier: TierMedium, Description: "GitHub Actions workflows", MinHumanApprovals: &one},
		{Paths: []string{DeclarationPath}, MinTier: TierMedium, Description: "risk declaration", MinHumanApprovals: &one},
	}
}
