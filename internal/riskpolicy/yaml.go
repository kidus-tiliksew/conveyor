package riskpolicy

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// strictMapping checks a YAML mapping before it is decoded: every key must be
// one of allowed, and every key in integers must hold an integer scalar.
// yaml.v3 truncates a float into an int field and drops KnownFields inside a
// custom unmarshaler, so the risk schemas check both here. It returns a
// *yaml.TypeError whose entries take yaml.v3's own forms, so configuration
// error redaction treats them as it treats the decoder's, and no entry
// carries a value from the document.
func strictMapping(node *yaml.Node, typeName string, allowed []string, integers ...string) error {
	if node.Kind != yaml.MappingNode {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: cannot unmarshal %s into %s", node.Line, node.ShortTag(), typeName)}}
	}
	known := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		known[key] = true
	}
	integer := make(map[string]bool, len(integers))
	for _, key := range integers {
		integer[key] = true
	}
	var problems []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if !known[key.Value] {
			problems = append(problems, fmt.Sprintf("line %d: field %s not found in type %s", key.Line, key.Value, typeName))
			continue
		}
		if integer[key.Value] && !(value.Kind == yaml.ScalarNode && (value.ShortTag() == "!!int" || value.ShortTag() == "!!null")) {
			problems = append(problems, fmt.Sprintf("line %d: cannot unmarshal %s into int", value.Line, value.ShortTag()))
		}
	}
	if len(problems) != 0 {
		return &yaml.TypeError{Errors: problems}
	}
	return nil
}

// UnmarshalYAML refuses unknown keys and non-integer counts in a tier row.
func (r *SettingsRow) UnmarshalYAML(node *yaml.Node) error {
	if err := strictMapping(node, "riskpolicy.SettingsRow", []string{"agent_reviews", "human_approvals"}, "agent_reviews", "human_approvals"); err != nil {
		return err
	}
	type plain SettingsRow
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*r = SettingsRow(decoded)
	return nil
}

// UnmarshalYAML refuses unknown keys and a non-integer min_human_approvals in
// a declaration rule (AC-3.1).
func (r *Rule) UnmarshalYAML(node *yaml.Node) error {
	if err := strictMapping(node, "riskpolicy.Rule", []string{"paths", "min_tier", "description", "min_human_approvals"}, "min_human_approvals"); err != nil {
		return err
	}
	type plain Rule
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*r = Rule(decoded)
	return nil
}
