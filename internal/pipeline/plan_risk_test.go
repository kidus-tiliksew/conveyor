package pipeline

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// riskPlan builds a plan whose Risks section holds risks verbatim.
func riskPlan(risks string) string {
	return "## Approach\n\nDo it.\n\n## Files touched\n\n- a.go\n\n## Ordering\n\n1. a\n\n## Risks\n\n" + risks + "\n\n## Done criteria\n\n- done\n"
}

// TestParsePlanRiskTierGrammar covers the risk tier line every plan path
// shares (req-risk-based-review REQ-4; feature-risk-based-review RR-4).
func TestParsePlanRiskTierGrammar(t *testing.T) {
	accepted := []struct {
		name   string
		plan   string
		tier   riskpolicy.Tier
		reason string
	}{
		{"low", riskPlan("Risk tier: low — Docs only."), riskpolicy.TierLow, "Docs only."},
		{"medium", riskPlan("- Some risk.\n\nRisk tier: medium — Touches the store."), riskpolicy.TierMedium, "Touches the store."},
		{"high", riskPlan("Risk tier: high — Changes approval."), riskpolicy.TierHigh, "Changes approval."},
		{"surrounding whitespace", riskPlan("   Risk tier:   high   —   Spaced reason.   "), riskpolicy.TierHigh, "Spaced reason."},
		{"no spaces around the dash", riskPlan("Risk tier: low—Tight."), riskpolicy.TierLow, "Tight."},
		{"crlf line endings", strings.ReplaceAll(riskPlan("Risk tier: medium — Windows editor."), "\n", "\r\n"), riskpolicy.TierMedium, "Windows editor."},
		{"nested heading under risks", riskPlan("### Tier\n\nRisk tier: high — Nested.\n"), riskpolicy.TierHigh, "Nested."},
		{"reason with a dash", riskPlan("Risk tier: low — One — two."), riskpolicy.TierLow, "One — two."},
		{"risks heading with hashes closed", strings.Replace(riskPlan("Risk tier: low — Closed heading."), "## Risks", "## Risks ##", 1), riskpolicy.TierLow, "Closed heading."},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParsePlan(tc.plan, nil)
			if err != nil {
				t.Fatalf("ParsePlan() error = %v", err)
			}
			if spec.RiskTier != tc.tier || spec.RiskReason != tc.reason {
				t.Fatalf("tier, reason = %q, %q; want %q, %q", spec.RiskTier, spec.RiskReason, tc.tier, tc.reason)
			}
		})
	}

	absent := []struct {
		name string
		plan string
	}{
		{"no declaration", riskPlan("- Ordinary risk.")},
		{"backtick fenced example", riskPlan("```text\nRisk tier: high — Example only.\n```")},
		{"tilde fenced example", riskPlan("~~~\nRisk tier: urgent — Example only.\n~~~")},
		{"fenced example outside risks", strings.Replace(riskPlan("- Ordinary."), "Do it.", "```\nRisk tier: bad\n```", 1)},
		{"prose that names the tier", riskPlan("- The risk tier: the reviewers set it later.")},
	}
	for _, tc := range absent {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParsePlan(tc.plan, nil)
			if err != nil {
				t.Fatalf("ParsePlan() error = %v", err)
			}
			if spec.RiskTier != "" || spec.RiskReason != "" {
				t.Fatalf("tier, reason = %q, %q; want none", spec.RiskTier, spec.RiskReason)
			}
		})
	}

	refused := []struct {
		name string
		plan string
		want string
	}{
		{"uppercase tier", riskPlan("Risk tier: High — Shouted."), "must be low, medium, or high in lowercase"},
		{"unknown tier", riskPlan("Risk tier: critical — No such tier."), "must be low, medium, or high"},
		{"missing tier", riskPlan("Risk tier: — No tier."), "must be low, medium, or high"},
		{"hyphen separator", riskPlan("Risk tier: low - Hyphen."), "em dash"},
		{"missing separator", riskPlan("Risk tier: low"), "em dash"},
		{"empty reason", riskPlan("Risk tier: low —"), "non-blank"},
		{"blank reason", riskPlan("Risk tier: low —    "), "non-blank"},
		{"too long reason", riskPlan("Risk tier: low — " + strings.Repeat("x", 1001)), "at most 1000"},
		{"list marker", riskPlan("- Risk tier: low — Listed."), "list marker"},
		{"bold label", riskPlan("**Risk tier:** low — Bold."), "list marker"},
		{"lowercase label", riskPlan("risk tier: low — Lower."), "must start with"},
		{"duplicate", riskPlan("Risk tier: low — One.\n\nRisk tier: high — Two."), "more than once"},
		{"outside risks", strings.Replace(riskPlan("- Ordinary."), "Do it.", "Risk tier: low — Wrong section.", 1), "under the Risks heading"},
		{"after risks ends", strings.Replace(riskPlan("- Ordinary."), "- done", "- done\n\nRisk tier: low — In done criteria.", 1), "under the Risks heading"},
		{"sibling heading ends risks", riskPlan("## Notes\n\nRisk tier: low — Under notes."), "under the Risks heading"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlan(tc.plan, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParsePlan() error = %v; want it to contain %q", err, tc.want)
			}
			// A malformed line never reaches a recorded rating, and an
			// earlier version that holds one stays readable as a plan.
			stored, storedErr := ParseStoredPlan(tc.plan)
			if storedErr != nil || stored.RiskTier != "" {
				t.Fatalf("ParseStoredPlan() = %q, %v; want a plan with no tier", stored.RiskTier, storedErr)
			}
		})
	}
}

// TestParsePlanKeepsStructureRules shows the tier grammar adds to, and never
// replaces, the existing plan checks.
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
	if tier, reason := PlanRiskOf("not a plan\n\nRisk tier: high — Prose."); tier != "" || reason != "" {
		t.Fatalf("PlanRiskOf(non-plan) = %q, %q; want none", tier, reason)
	}
	if tier, reason := PlanRiskOf(riskPlan("Risk tier: medium — Read back.")); tier != riskpolicy.TierMedium || reason != "Read back." {
		t.Fatalf("PlanRiskOf = %q, %q", tier, reason)
	}
}

// TestCheckPlanRiskTier covers each risk mode: enforce refuses a plan with no
// tier, record and off accept it, and a stated tier passes every mode
// (req-risk-based-review AC-4.1, AC-4.2, AC-4.5, AC-1.1).
func TestCheckPlanRiskTier(t *testing.T) {
	stated, err := ParsePlan(riskPlan("Risk tier: low — Stated."), nil)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := ParsePlan(riskPlan("- No tier."), nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]*riskpolicy.Policy{
		"absent":  nil,
		"off":     {Mode: riskpolicy.ModeOff, Tiers: riskpolicy.DefaultTable()},
		"record":  {Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()},
		"enforce": {Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()},
	}
	for name, policy := range policies {
		if err := CheckPlanRiskTier(policy, stated); err != nil {
			t.Fatalf("%s: stated tier refused: %v", name, err)
		}
		err := CheckPlanRiskTier(policy, missing)
		if name == "enforce" {
			if !errors.Is(err, ErrPlanRiskTierMissing) {
				t.Fatalf("enforce: missing tier error = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: missing tier refused: %v", name, err)
		}
	}
}

// TestStructuredPlanCarriesRiskTier shows submit_plan's structured path reads
// the tier from the markdown and takes no separate tier argument.
func TestStructuredPlanCarriesRiskTier(t *testing.T) {
	spec, err := RenderStructuredPlan(`{"markdown":` + jsonString(riskPlan("Risk tier: high — From markdown.")) + `,"decomposition":[]}`)
	if err != nil || spec.RiskTier != riskpolicy.TierHigh || spec.RiskReason != "From markdown." {
		t.Fatalf("RenderStructuredPlan = %q, %q, %v", spec.RiskTier, spec.RiskReason, err)
	}
	if _, err := RenderStructuredPlan(`{"markdown":` + jsonString(riskPlan("- x")) + `,"decomposition":[],"risk_tier":"high"}`); err == nil {
		t.Fatal("RenderStructuredPlan accepted a separate risk_tier field")
	}
	if _, err := RenderStructuredPlan(`{"markdown":` + jsonString(riskPlan("Risk tier: severe — Bad.")) + `,"decomposition":[]}`); err == nil {
		t.Fatal("RenderStructuredPlan accepted a malformed tier line")
	}
}

func jsonString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			builder.WriteRune(char)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
