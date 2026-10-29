package core

import (
	"reflect"
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

func TestResolveGovernedDesignsIsDeterministicAndDeduplicatesOverlappingScopes(t *testing.T) {
	designs := []GovernanceDesignContext{
		{ID: "design-z", Version: 2, Governs: []GovernedScope{{Repository: "app", Paths: []string{"internal/**", "internal/workorder/**"}}}},
		{ID: "design-a", Version: 1, Governs: []GovernedScope{{Repository: "other", Paths: []string{"**"}}}},
	}
	matches := ResolveGovernedDesigns(designs, "app", []string{"internal/workorder/service.go", "README.md", "internal/workorder/service.go"})
	if len(matches) != 1 || matches[0].Design.ID != "design-z" || len(matches[0].MatchingPaths) != 1 || matches[0].MatchingPaths[0] != "internal/workorder/service.go" {
		t.Fatalf("matches=%+v", matches)
	}
}

func TestGovernanceAssessmentNormalizesDisjointFindings(t *testing.T) {
	legacy := true
	assessment := GovernanceAssessment{Applicable: &legacy, CitedIDs: []string{"DEC-2", "DEC-1", "DEC-1"}, UnknownIDs: []string{"missing"}}
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

func TestGovernanceAssessmentSplitsDesignAndDecisionAuthority(t *testing.T) {
	design, decisions := false, true
	assessment := GovernanceAssessment{DesignApplicable: &design, DecisionCitable: &decisions, CitedIDs: []string{"DEC-2"}}
	if err := NormalizeGovernanceAssessment(&assessment); err != nil {
		t.Fatal(err)
	}
	if *assessment.DesignApplicable || !*assessment.DecisionCitable {
		t.Fatalf("normalized split=%+v", assessment)
	}
	legacy := true
	compat := GovernanceAssessment{Applicable: &legacy}
	if err := NormalizeGovernanceAssessment(&compat); err != nil || !*compat.DesignApplicable || !*compat.DecisionCitable || !compat.UsesLegacyApplicable() {
		t.Fatalf("legacy mapping=%+v err=%v", compat, err)
	}
	onlyDesign := true
	if err := NormalizeGovernanceAssessment(&GovernanceAssessment{DesignApplicable: &onlyDesign}); err == nil || !strings.Contains(err.Error(), "both design_applicable and decision_citable") {
		t.Fatalf("partial split error=%v", err)
	}
}

// TestDecisionTokensApplyTheWholeTokenRule pins the extraction the
// confirmation writers record as cited_decision_ids (component-document-corpus;
// DEC-90(2); req-260802-72fc68 AC-6.3).
func TestDecisionTokensApplyTheWholeTokenRule(t *testing.T) {
	content := "Cites DEC-40 and DEC-9, then DEC-10 (DEC-9 again). Not DEC-04, xDEC-3, DEC-5x, DEC-6_a, or DEC-. Ends with DEC-2."
	want := []string{"DEC-2", "DEC-9", "DEC-10", "DEC-40"}
	got := DecisionTokens(content)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecisionTokens=%v want %v", got, want)
	}
	for _, token := range got {
		if !ContainsDecisionToken(content, token) {
			t.Fatalf("token %s fails ContainsDecisionToken", token)
		}
	}
	for _, absent := range []string{"DEC-4", "DEC-1", "DEC-3", "DEC-5", "DEC-6"} {
		if ContainsDecisionToken(content, absent) {
			t.Fatalf("ContainsDecisionToken matched %s inside a longer token", absent)
		}
	}
	if got := DecisionTokens("no decisions here"); got == nil || len(got) != 0 {
		t.Fatalf("DecisionTokens without tokens=%#v, want an empty list", got)
	}
	if !IsDecisionID("DEC-90") || IsDecisionID("DEC-0") || IsDecisionID("component-lineage") || IsDecisionID(" DEC-1") {
		t.Fatal("IsDecisionID accepts only DEC-n")
	}
}

// TestDeriveDecisionCitations pins the decision citation read's derivation:
// whole tokens only, workspace decisions only, one item per pair, and the
// documented order (component-document-corpus, Decision citations;
// req-document-operating-surfaces AC-8.5).
func TestDeriveDecisionCitations(t *testing.T) {
	sources := []DecisionCitationSource{
		{Tier: DecisionSweepTierSystemDesign, DocumentID: "design-b", Title: "Shared", Version: 3, Content: "Cites DEC-4 and DEC-4 again, plus DEC-99."},
		{Tier: DecisionSweepTierSystemDesign, DocumentID: "design-a", Title: "Shared", Version: 2, Content: "Cites DEC-40 and DEC-4."},
		{Tier: DecisionSweepTierRequirement, DocumentID: "req-z", Title: "Zeta", Version: 1, Content: "Cites DEC-40 only; DEC-400x and xDEC-4 do not count."},
		{Tier: DecisionSweepTierRequirement, DocumentID: "req-a", Title: "Alpha", Version: 5, Content: "Cites DEC-4."},
		{Tier: DecisionSweepTierRequirement, DocumentID: "req-none", Title: "None", Version: 1, Content: "Names no decision."},
	}
	got := DeriveDecisionCitations([]string{"DEC-40", "DEC-4"}, sources)
	want := []DecisionCitation{
		{DecisionID: "DEC-4", Tier: DecisionSweepTierRequirement, DocumentID: "req-a", Title: "Alpha", Version: 5},
		{DecisionID: "DEC-4", Tier: DecisionSweepTierSystemDesign, DocumentID: "design-a", Title: "Shared", Version: 2},
		{DecisionID: "DEC-4", Tier: DecisionSweepTierSystemDesign, DocumentID: "design-b", Title: "Shared", Version: 3},
		{DecisionID: "DEC-40", Tier: DecisionSweepTierRequirement, DocumentID: "req-z", Title: "Zeta", Version: 1},
		{DecisionID: "DEC-40", Tier: DecisionSweepTierSystemDesign, DocumentID: "design-a", Title: "Shared", Version: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveDecisionCitations=%+v\nwant %+v", got, want)
	}
	if empty := DeriveDecisionCitations(nil, sources); empty == nil || len(empty) != 0 {
		t.Fatalf("no decisions = %#v, want a non-nil empty list", empty)
	}
}
