package riskpolicy

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// nonDecimalIntegers are YAML spellings that yaml.v3 would convert to an int,
// by truncation or by another base, and that the risk schemas refuse.
var nonDecimalIntegers = []string{
	"1.5", "1.0", "1e0", "0.9", `"1"`, `'1'`, `!!int "1"`, "0x1", "0o1", "0b1", "+1", "01", "1_000",
	"[1]", "{n: 1}", "true", "one",
}

// req-risk-based-review AC-3.1: schema_version and min_human_approvals must be
// plain decimal integers; yaml.v3 alone reads schema_version: 1.5 as 1.
func TestParseDeclarationRefusesNonDecimalIntegers(t *testing.T) {
	const rule = "rules:\n  - paths: [internal/**]\n    min_tier: high\n    description: internal\n"
	for _, value := range nonDecimalIntegers {
		t.Run("schema_version "+value, func(t *testing.T) {
			content := "schema_version: " + value + "\n" + rule
			if declaration, err := ParseDeclaration([]byte(content)); err == nil {
				t.Fatalf("schema_version %s was accepted as %+v", value, declaration)
			}
		})
		t.Run("min_human_approvals "+value, func(t *testing.T) {
			content := "schema_version: 1\n" + rule + "    min_human_approvals: " + value + "\n"
			if declaration, err := ParseDeclaration([]byte(content)); err == nil {
				t.Fatalf("min_human_approvals %s was accepted as %+v", value, declaration.Rules[0].MinHumanApprovals)
			}
		})
	}
	// Integer version 1 keeps working, and a refusal names the tag, never the value.
	if declaration, err := ParseDeclaration([]byte("schema_version: 1\nrules: []\n")); err != nil || declaration.SchemaVersion != 1 {
		t.Fatalf("integer version 1=%+v, %v", declaration, err)
	}
	if _, err := ParseDeclaration([]byte("schema_version: 1.5\nrules: []\n")); err == nil || !strings.Contains(err.Error(), "cannot unmarshal !!float into int") {
		t.Fatalf("schema_version 1.5 err=%v", err)
	}
	if _, err := ParseDeclaration([]byte("schema_version: 0x1\nrules: []\n")); err == nil || !strings.Contains(err.Error(), "riskpolicy.DecimalInt") || strings.Contains(err.Error(), "0x1") {
		t.Fatalf("schema_version 0x1 err=%v", err)
	}
	// The closed schema still holds at the top level after the custom decoder.
	if _, err := ParseDeclaration([]byte("schema_version: 1\nrules: []\nowners: []\n")); err == nil || !strings.Contains(err.Error(), "owners") {
		t.Fatalf("unknown top-level key err=%v", err)
	}
	for _, value := range []string{"0", "2", "-0"} {
		content := "schema_version: 1\n" + rule + "    min_human_approvals: " + value + "\n"
		if _, err := ParseDeclaration([]byte(content)); err != nil {
			t.Fatalf("min_human_approvals %s refused: %v", value, err)
		}
	}
	// An explicit null reads as an omitted min_human_approvals.
	declaration, err := ParseDeclaration([]byte("schema_version: 1\n" + rule + "    min_human_approvals: null\n"))
	if err != nil || declaration.Rules[0].MinHumanApprovals != nil {
		t.Fatalf("null min_human_approvals=%+v, %v", declaration, err)
	}
}

// AC-3.5; component-risk-review invariant 7: declaration bytes with a
// fractional schema version are malformed, so the head takes high with a
// recorded reason, never the declared or built-in minimum alone.
func TestPathMinimumsFractionalVersionTakesHigh(t *testing.T) {
	for _, content := range []string{
		"schema_version: 1.5\nrules: []\n",
		"schema_version: 1.0\nrules: []\n",
		"schema_version: 1\nrules:\n  - {paths: [docs/**], min_tier: low, description: docs, min_human_approvals: 1.5}\n",
	} {
		got := PathMinimums(PathInput{Declaration: DeclarationInput{State: DeclarationPresent, Bytes: []byte(content)}, ChangedPaths: []string{"docs/a.md"}, ChangedPathsComplete: true})
		if got.Tier != TierHigh || len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "declaration is malformed") {
			t.Fatalf("PathMinimums(%q)=%+v, want high with a malformed-declaration reason", content, got)
		}
	}
}

// The workspace risk_policy table refuses the same spellings in YAML
// (req-risk-based-review AC-1.2).
func TestSettingsRowRefusesNonDecimalIntegers(t *testing.T) {
	for _, value := range nonDecimalIntegers {
		for _, key := range []string{"agent_reviews", "human_approvals"} {
			other := map[string]string{"agent_reviews": "human_approvals", "human_approvals": "agent_reviews"}[key]
			content := "mode: record\ntiers:\n  low: {" + key + ": " + value + ", " + other + ": 1}\n"
			decoder := yaml.NewDecoder(strings.NewReader(content))
			decoder.KnownFields(true)
			var settings Settings
			if err := decoder.Decode(&settings); err == nil {
				t.Fatalf("%s %s was accepted", key, value)
			}
		}
	}
	var settings Settings
	if err := yaml.Unmarshal([]byte("mode: record\ntiers:\n  low: {agent_reviews: 1, human_approvals: 0}\n"), &settings); err != nil || *settings.Tiers.Low.AgentReviews != 1 {
		t.Fatalf("decimal row=%+v, %v", settings.Tiers, err)
	}
}
