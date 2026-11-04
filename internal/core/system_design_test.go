package core

import (
	"encoding/json"
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

// A declared repository with an empty path list governs no paths; every other
// fence rule stays enforced (req-task-authority-boundary AC-5.1; DEC-101(5)).
func TestParseGovernedScopesAcceptsEmptyPathList(t *testing.T) {
	for name, test := range map[string]struct {
		fence string
		want  []GovernedScope
	}{
		"flow list": {"- repo: conveyor\n  paths: []", []GovernedScope{{Repository: "conveyor", Paths: []string{}}}},
		"empty beside non-empty scope": {
			"- repo: other\n  paths: [cmd/**, internal/**]\n- repo: conveyor\n  paths: []",
			[]GovernedScope{{Repository: "conveyor", Paths: []string{}}, {Repository: "other", Paths: []string{"cmd/**", "internal/**"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			content := "# Design\n\n```conveyor:governs\n" + test.fence + "\n```"
			scopes, err := ParseGovernedScopes(content)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(scopes, test.want) {
				t.Fatalf("scopes=%#v want %#v", scopes, test.want)
			}
			version := SystemDesignVersion{DocumentID: "feature-demo", Content: content, Origin: SystemDesignOriginOperator}
			if err = NormalizeSystemDesignVersion(&version); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(version.Governs, test.want) {
				t.Fatalf("normalized governs=%#v want %#v", version.Governs, test.want)
			}
			encoded, err := json.Marshal(version.Governs[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"paths":[]`) {
				t.Fatalf("empty list must persist as an array: %s", encoded)
			}
		})
	}
	for name, test := range map[string]struct{ content, reason string }{
		"no fence":          {"# Design", "exactly one conveyor:governs fence"},
		"two empty fences":  {"```conveyor:governs\n- repo: conveyor\n  paths: []\n```\n```conveyor:governs\n- repo: other\n  paths: []\n```", "exactly one conveyor:governs fence"},
		"no repository":     {"```conveyor:governs\n[]\n```", "at least one repository scope"},
		"absent paths":      {"```conveyor:governs\n- repo: conveyor\n```", "must declare a paths list"},
		"null paths":        {"```conveyor:governs\n- repo: conveyor\n  paths:\n```", "must declare a paths list"},
		"misspelled key":    {"```conveyor:governs\n- repo: conveyor\n  path: [internal/**]\n```", "must declare a paths list"},
		"duplicate empty":   {"```conveyor:governs\n- repo: conveyor\n  paths: []\n- repo: conveyor\n  paths: []\n```", "declared more than once"},
		"invalid repo name": {"```conveyor:governs\n- repo: acme/conveyor\n  paths: []\n```", "configured repository name"},
		"escaping glob":     {"```conveyor:governs\n- repo: conveyor\n  paths: [../secret]\n```", "must be repository-relative"},
		"character class":   {"```conveyor:governs\n- repo: conveyor\n  paths: [\"src/[ab]/*.go\"]\n```", "unsupported character-class syntax"},
		"blank glob":        {"```conveyor:governs\n- repo: conveyor\n  paths: [\"\"]\n```", "must be repository-relative"},
		"duplicate glob":    {"```conveyor:governs\n- repo: conveyor\n  paths: [cmd/**, cmd/**]\n```", "is duplicated"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGovernedScopes(test.content); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error=%v want %q", err, test.reason)
			}
		})
	}
}

// An empty list matches nothing, in its own repository and across repositories
// (req-task-authority-boundary REQ-5, AC-5.2).
func TestEmptyPathListIsNotARepositoryWildcard(t *testing.T) {
	noPath := GovernanceDesignContext{ID: "no-path", Governs: []GovernedScope{{Repository: "conveyor", Paths: []string{}}}}
	emptyHere := GovernanceDesignContext{ID: "empty-here", Governs: []GovernedScope{{Repository: "conveyor", Paths: []string{}}, {Repository: "other", Paths: []string{"**"}}}}
	emptyElsewhere := GovernanceDesignContext{ID: "empty-elsewhere", Governs: []GovernedScope{{Repository: "conveyor", Paths: []string{"internal/**"}}, {Repository: "other", Paths: []string{}}}}
	designs := []GovernanceDesignContext{noPath, emptyHere, emptyElsewhere}
	matchIDs := func(repository string, changed ...string) []string {
		ids := []string{}
		for _, match := range ResolveGovernedDesigns(designs, repository, changed) {
			ids = append(ids, match.Design.ID)
		}
		return ids
	}
	if got := matchIDs("conveyor", "internal/core/types.go", "README.md"); !reflect.DeepEqual(got, []string{"empty-elsewhere"}) {
		t.Fatalf("conveyor matches=%v", got)
	}
	if got := matchIDs("other", "internal/core/types.go"); !reflect.DeepEqual(got, []string{"empty-here"}) {
		t.Fatalf("other matches=%v", got)
	}
	for _, test := range []struct {
		design     GovernanceDesignContext
		repository string
		want       bool
	}{
		{noPath, "conveyor", false},
		{noPath, "other", false},
		{emptyHere, "conveyor", false},
		{emptyHere, "other", true},
		{emptyElsewhere, "conveyor", true},
		{emptyElsewhere, "other", false},
	} {
		if got := GovernsRepositoryPaths(test.design.Governs, test.repository); got != test.want {
			t.Errorf("GovernsRepositoryPaths(%s,%s)=%t want %t", test.design.ID, test.repository, got, test.want)
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
