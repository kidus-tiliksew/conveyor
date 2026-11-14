package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

// riskPlan builds a plan whose Risks section holds risks verbatim.
func riskPlan(risks string) string {
	return "## Approach\n\nDo it.\n\n## Files touched\n\n- a.go\n\n## Ordering\n\n1. a\n\n## Risks\n\n" + risks + "\n\n## Done criteria\n\n- done\n"
}

// TestPlanTierLineIsPlanText shows that no plan parser reads a tier line: a
// plan with a `Risk tier:` line and a plan without one are both accepted, and
// every line the retired grammar read or refused, well-formed, malformed,
// repeated, decorated, or misplaced, is ordinary plan text kept verbatim
// (req-risk-based-review REQ-4; feature-risk-based-review RR-13).
func TestPlanTierLineIsPlanText(t *testing.T) {
	cases := []struct {
		name string
		plan string
	}{
		{"no tier line", riskPlan("- Ordinary risk.")},
		{"well-formed line", riskPlan("Risk tier: high — Changes approval.")},
		{"uppercase tier", riskPlan("Risk tier: High — Shouted.")},
		{"unknown tier", riskPlan("Risk tier: critical — No such tier.")},
		{"missing tier", riskPlan("Risk tier: — No tier.")},
		{"hyphen separator", riskPlan("Risk tier: low - Hyphen.")},
		{"missing separator", riskPlan("Risk tier: low")},
		{"empty reason", riskPlan("Risk tier: low —")},
		{"too long reason", riskPlan("Risk tier: low — " + strings.Repeat("x", 1001))},
		{"list marker", riskPlan("- Risk tier: low — Listed.")},
		{"bold label", riskPlan("**Risk tier:** low — Bold.")},
		{"lowercase label", riskPlan("risk tier: low — Lower.")},
		{"duplicate", riskPlan("Risk tier: low — One.\n\nRisk tier: high — Two.")},
		{"outside risks", strings.Replace(riskPlan("- Ordinary."), "Do it.", "Risk tier: low — Wrong section.", 1)},
		{"after risks ends", strings.Replace(riskPlan("- Ordinary."), "- done", "- done\n\nRisk tier: low — In done criteria.", 1)},
		{"fenced example", riskPlan("```text\nRisk tier: high — Example only.\n```")},
		{"crlf line endings", strings.ReplaceAll(riskPlan("Risk tier: medium — Windows editor."), "\n", "\r\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParsePlan(tc.plan, nil)
			if err != nil {
				t.Fatalf("ParsePlan() error = %v", err)
			}
			want := strings.TrimSpace(strings.ReplaceAll(tc.plan, "\r\n", "\n"))
			if spec.Markdown != want {
				t.Fatalf("ParsePlan() changed the plan text:\n%q\nwant\n%q", spec.Markdown, want)
			}
			if _, err := ParseStoredPlan(tc.plan); err != nil {
				t.Fatalf("ParseStoredPlan() error = %v", err)
			}
			if _, err := RenderStructuredPlan(`{"markdown":` + jsonString(tc.plan) + `,"decomposition":[]}`); err != nil {
				t.Fatalf("RenderStructuredPlan() error = %v", err)
			}
		})
	}
}

// TestParsePlanKeepsStructureRules shows that removing the tier grammar keeps
// every other plan check.
func TestParsePlanKeepsStructureRules(t *testing.T) {
	withoutRisks := strings.Replace(riskPlan("Risk tier: low — Fine."), "## Risks", "## Hazards", 1)
	if _, err := ParsePlan(withoutRisks, nil); err == nil || !strings.Contains(err.Error(), "risks heading") {
		t.Fatalf("ParsePlan(no Risks heading) error = %v", err)
	}
	if _, err := ParsePlan(riskPlan("Risk tier: low — Fine."), []DecompositionItem{{ID: "SUB-1"}}); err == nil {
		t.Fatal("ParsePlan(decomposition) accepted a decomposition")
	}
	fenced := riskPlan("Risk tier: low — Fine.\n\n```conveyor:acceptance\n[]\n```")
	if _, err := ParsePlan(fenced, nil); err == nil || !strings.Contains(err.Error(), "machine fences") {
		t.Fatalf("ParsePlan(machine fence) error = %v", err)
	}
	if _, err := ParseStoredPlan("not a plan"); err == nil {
		t.Fatal("ParseStoredPlan accepted content that is not a plan")
	}
	if _, err := RenderStructuredPlan(`{"markdown":` + jsonString(riskPlan("- x")) + `,"decomposition":[],"risk_tier":"high"}`); err == nil {
		t.Fatal("RenderStructuredPlan accepted a separate risk_tier field")
	}
}

func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
