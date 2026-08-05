package core

import (
	"strings"
	"testing"
)

func TestSystemDesignGovernedScopeParsingAndMatching(t *testing.T) {
	version := SystemDesignVersion{Content: "# Runtime\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - cmd/*.go\n```", Origin: SystemDesignOriginOperator}
	if err := NormalizeSystemDesignVersion(&version); err != nil {
		t.Fatal(err)
	}
	if len(version.Governs) != 1 || len(version.Governs[0].Paths) != 2 {
		t.Fatalf("governs=%+v", version.Governs)
	}
	for _, test := range []struct {
		glob, path string
		want       bool
	}{
		{"internal/dispatch/**", "internal/dispatch/dispatch.go", true},
		{"internal/dispatch/**", "internal/dispatch/nested/worker.go", true},
		{"cmd/*.go", "cmd/conveyor/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/core/types.go", true},
		{"**", "../outside", false},
	} {
		if got := MatchGovernedPath(test.glob, test.path); got != test.want {
			t.Errorf("MatchGovernedPath(%q,%q)=%t want %t", test.glob, test.path, got, test.want)
		}
	}
	accepted := []struct {
		glob, changed string
		want          bool
	}{
		{"internal/*/service?.go", "internal/dispatch/service1.go", true},
		{"internal/*/service?.go", "internal/dispatch/nested/service1.go", false},
		{"internal/**/service?.go", "internal/service1.go", true},
		{"internal/**/service?.go", "internal/dispatch/service1.go", true},
	}
	for _, test := range accepted {
		content := "# Design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - " + test.glob + "\n```"
		scopes, err := ParseGovernedScopes(content)
		if err != nil {
			t.Fatalf("ParseGovernedScopes(%q): %v", test.glob, err)
		}
		if got := MatchGovernedPath(scopes[0].Paths[0], test.changed); got != test.want {
			t.Errorf("validated MatchGovernedPath(%q,%q)=%t want %t", test.glob, test.changed, got, test.want)
		}
	}
	for _, content := range []string{
		"# Missing",
		"```conveyor:governs\n- repo: conveyor\n  paths: []\n```",
		"```conveyor:governs\n- repo: conveyor\n  paths: [../secret]\n```",
		"```conveyor:governs\n- repo: conveyor\n  paths: [internal/**]\n```\n```conveyor:governs\n- repo: other\n  paths: [cmd/**]\n```",
	} {
		if _, err := ParseGovernedScopes(content); err == nil {
			t.Fatalf("invalid design accepted: %q", content)
		}
	}
	for _, glob := range []string{"internal/[ab]/**", "internal/[]/**"} {
		content := "# Design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - " + glob + "\n```"
		if _, err := ParseGovernedScopes(content); err == nil || !strings.Contains(err.Error(), "unsupported character-class syntax") {
			t.Fatalf("character-class glob %q error=%v", glob, err)
		}
	}
}

func TestGovernanceAssessmentNormalizesDisjointFindings(t *testing.T) {
	assessment := GovernanceAssessment{Applicable: true, CitedIDs: []string{"DEC-2", "DEC-1", "DEC-1"}, UnknownIDs: []string{"missing"}}
	if err := NormalizeGovernanceAssessment(&assessment); err != nil {
		t.Fatal(err)
	}
	if len(assessment.CitedIDs) != 2 || assessment.CitedIDs[0] != "DEC-1" || assessment.CitedIDs[1] != "DEC-2" {
		t.Fatalf("normalized=%+v", assessment)
	}
	assessment.Conflicts = []string{"missing"}
	if err := NormalizeGovernanceAssessment(&assessment); err == nil {
		t.Fatal("overlapping governance findings were accepted")
	}
}
