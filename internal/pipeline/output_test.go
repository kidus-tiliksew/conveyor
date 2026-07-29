package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseTriageAndReview(t *testing.T) {
	triage, err := ParseTriage("```conveyor:triage\n{\"class\":\"feature\",\"route\":\"spec\",\"summary\":\"Needs a contract.\"}\n```")
	if err != nil || triage.Route != "spec" {
		t.Fatalf("triage=%+v err=%v", triage, err)
	}
	review, err := ParseReview("```conveyor:review\n{\"verdict\":\"changes_requested\",\"reason_code\":\"scope-creep\",\"summary\":\"Extra refactor\",\"feedback\":\"Remove it.\"}\n```")
	if err != nil || review.ReasonCode != "scope-creep" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
}

func TestParseSpecValidatesMachineBlocks(t *testing.T) {
	valid := `# Change

## Intent
Ship it.

## Non-goals
Unrelated work.

` + "```conveyor:acceptance\n- id: AC-1\n  criterion: Tests pass\n  verify: test\n  ref: ./...\n```" + `

` + "```conveyor:decomposition\n- id: SUB-1\n  repo: api\n  summary: Add the contract\n  depends_on: []\n```"
	parsed, err := ParseSpec(valid)
	if err != nil || len(parsed.Acceptance) != 1 || len(parsed.Decomposition) != 1 {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	invalid := `# Change

## Intent
Ship it.

## Non-goals
Nothing.

` + "```conveyor:acceptance\n- id: AC-1\n  criterion: Tests pass\n  verify: test\n```" + `

` + "```conveyor:decomposition\n- id: SUB-1\n  repo: api\n  summary: Add it\n  depends_on: [SUB-404]\n```"
	if _, err := ParseSpec(invalid); err == nil {
		t.Fatal("accepted an unknown decomposition dependency")
	}
}

func TestParseSpecTreatsMermaidAndOtherFencesAsOrdinaryProse(t *testing.T) {
	document := "## Intent\n\n```mermaid\nthis is deliberately malformed\n```\n\n```go\nfunc example() {}\n```\n\n## Non-goals\n\nNone.\n\n```conveyor:acceptance\n- id: AC-1\n  criterion: Works\n  verify: test\n```"
	if _, err := ParseSpec(document); err != nil {
		t.Fatalf("ordinary prose fence failed validation: %v", err)
	}
}

func TestRenderStructuredSpecProducesCanonicalMachineBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		decomposition []DecompositionItem
		wantFence     bool
	}{
		{name: "acceptance only"},
		{name: "acceptance and decomposition", decomposition: []DecompositionItem{{ID: "SUB-1", Repo: "api", Summary: "Ship the API", DependsOn: []string{}}}, wantFence: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(StructuredSpec{
				Markdown:      "# Canonical\n\n## Intent\nShip it.\n\n## Non-goals\nNo unrelated work.",
				Acceptance:    []AcceptanceCriterion{{ID: "AC-1", Criterion: "Tests pass", Verify: "test"}},
				Decomposition: test.decomposition,
			})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := RenderStructuredSpec(string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(parsed.Markdown, "```conveyor:acceptance\n") != 1 || strings.Contains(parsed.Markdown, "```yaml conveyor:acceptance") || strings.Contains(parsed.Markdown, "criteria:") {
				t.Fatalf("non-canonical acceptance render:\n%s", parsed.Markdown)
			}
			if strings.Contains(parsed.Markdown, "\n  ref:") {
				t.Fatalf("empty optional ref was rendered:\n%s", parsed.Markdown)
			}
			if got := strings.Contains(parsed.Markdown, "```conveyor:decomposition\n"); got != test.wantFence {
				t.Fatalf("decomposition fence present=%t want=%t:\n%s", got, test.wantFence, parsed.Markdown)
			}
			roundTrip, err := ParseSpec(parsed.Markdown)
			if err != nil || len(roundTrip.Acceptance) != 1 || len(roundTrip.Decomposition) != len(test.decomposition) {
				t.Fatalf("roundTrip=%+v err=%v", roundTrip, err)
			}
		})
	}
}

func TestParseSpecRejectsGrokNearMissWithShapeDiagnostic(t *testing.T) {
	t.Parallel()
	nearMiss := "# Near miss\n\n## Intent\nShip it.\n\n## Non-goals\nNone.\n\n```yaml conveyor:acceptance\ncriteria:\n  - id: AC-1\n    criterion: Tests pass\n    verify: test\n```"
	_, err := ParseSpec(nearMiss)
	if err == nil || !strings.Contains(err.Error(), "exact ```conveyor:acceptance fence") || !strings.Contains(err.Error(), "```yaml conveyor:acceptance") {
		t.Fatalf("near-miss error = %v", err)
	}

	wrongShape := "# Wrapper\n\n## Intent\nShip it.\n\n## Non-goals\nNone.\n\n```conveyor:acceptance\ncriteria:\n  - id: AC-1\n    criterion: Tests pass\n    verify: test\n```"
	_, err = ParseSpec(wrongShape)
	if err == nil || !strings.Contains(err.Error(), "top-level YAML list") || !strings.Contains(err.Error(), `wrapper key "criteria"`) {
		t.Fatalf("wrapper error = %v", err)
	}
}

func TestRenderStructuredSpecNamesInvalidSemanticShape(t *testing.T) {
	t.Parallel()
	markdown := "# Invalid\n\n## Intent\nShip it.\n\n## Non-goals\nNone."
	tests := []struct {
		name       string
		acceptance []AcceptanceCriterion
		want       string
	}{
		{name: "invalid verify", acceptance: []AcceptanceCriterion{{ID: "AC-1", Criterion: "Ship", Verify: "tests"}}, want: `invalid verify value "tests"`},
		{name: "invalid id", acceptance: []AcceptanceCriterion{{ID: "AC-0", Criterion: "Ship", Verify: "test"}}, want: `invalid id "AC-0"`},
		{name: "duplicate id", acceptance: []AcceptanceCriterion{{ID: "AC-1", Criterion: "Ship", Verify: "test"}, {ID: "AC-1", Criterion: "Test", Verify: "test"}}, want: `duplicate id "AC-1"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(StructuredSpec{Markdown: markdown, Acceptance: test.acceptance, Decomposition: []DecompositionItem{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RenderStructuredSpec(string(encoded)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	wrongType := `{"markdown":"# Invalid\n\n## Intent\nShip it.\n\n## Non-goals\nNone.","acceptance":{"criteria":[]},"decomposition":[]}`
	if _, err := RenderStructuredSpec(wrongType); err == nil || !strings.Contains(err.Error(), "structured spec output must match the required object shape") || !strings.Contains(err.Error(), "cannot unmarshal object") {
		t.Fatalf("wrong-type error = %v", err)
	}
}

func TestRenderStructuredSpecRejectsModelAuthoredMachineFence(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(StructuredSpec{
		Markdown:      "# Invalid\n\n## Intent\nShip it.\n\n## Non-goals\nNone.\n\n```yaml conveyor:acceptance\ncriteria: []\n```",
		Acceptance:    []AcceptanceCriterion{{ID: "AC-1", Criterion: "Ship", Verify: "test"}},
		Decomposition: []DecompositionItem{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RenderStructuredSpec(string(encoded)); err == nil || !strings.Contains(err.Error(), "must not contain model-authored conveyor machine fences") {
		t.Fatalf("error = %v", err)
	}
}

func TestRenderStructuredSpecRejectsDecompositionCycle(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(StructuredSpec{
		Markdown:   "# Cycle\n\n## Intent\nShip it.\n\n## Non-goals\nNone.",
		Acceptance: []AcceptanceCriterion{{ID: "AC-1", Criterion: "No cycle", Verify: "test"}},
		Decomposition: []DecompositionItem{
			{ID: "SUB-1", Repo: "conveyor", Summary: "one", DependsOn: []string{"SUB-2"}},
			{ID: "SUB-2", Repo: "conveyor", Summary: "two", DependsOn: []string{"SUB-1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RenderStructuredSpec(string(encoded)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error=%v", err)
	}
}
