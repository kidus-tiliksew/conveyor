package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// PlanRiskTierLabel opens a plan's risk tier declaration. The whole line is
// `Risk tier: <low|medium|high> — <reason>` (req-risk-based-review REQ-4;
// feature-risk-based-review RR-4).
const PlanRiskTierLabel = "Risk tier:"

// PlanRiskTierSeparator separates the tier from its reason.
const PlanRiskTierSeparator = "—"

// PlanRiskTierFormat is the declaration grammar that refusals name.
const PlanRiskTierFormat = "`Risk tier: <low|medium|high> — <reason>`"

// ErrPlanRiskTierMissing refuses a plan that states no risk tier while the
// task's risk mode is enforce (req-risk-based-review AC-4.1, AC-4.2).
var ErrPlanRiskTierMissing = errors.New("plan_risk_tier_missing: the task's risk policy is in enforce mode, so the plan must state its risk tier as one line under its Risks heading: " + PlanRiskTierFormat)

var (
	planATXHeading = regexp.MustCompile(`^(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	// A list marker, block quote, or emphasis before the label still marks a
	// reserved declaration, so a decorated line is refused instead of being
	// ignored as prose.
	planRiskDecoration = regexp.MustCompile(`^(?:(?:[-*+]|[0-9]+[.)])[ \t]+|>[ \t]*|[*_]+)+`)
)

// planRiskDeclaration is one line that claims the reserved label.
type planRiskDeclaration struct {
	line      int
	text      string
	underRisk bool
}

// parsePlanRiskTier finds and validates the plan's risk tier declaration. A
// plan without one is valid here; the caller that knows the task's risk mode
// decides whether a missing tier is refused. Fenced code blocks are skipped,
// so an example in a fence neither states nor breaks the tier. A reserved
// declaration that is malformed, repeated, or outside the Risks section is
// refused on every path (feature-risk-based-review RR-4).
func parsePlanRiskTier(markdown string) (riskpolicy.Tier, string, error) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var found []planRiskDeclaration
	fence := ""
	riskLevel := 0
	for index, raw := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if opener := fenceOpener(trimmed); opener != "" {
			fence = opener
			continue
		}
		if match := planATXHeading.FindStringSubmatch(trimmed); match != nil && leadingSpaces(raw) < 4 {
			level := len(match[1])
			if riskLevel > 0 && level <= riskLevel {
				riskLevel = 0
			}
			if planHeadingName(match[2]) == "risks" {
				riskLevel = level
			}
			continue
		}
		undecorated := planRiskDecoration.ReplaceAllString(trimmed, "")
		if !strings.HasPrefix(strings.ToLower(undecorated), strings.ToLower(PlanRiskTierLabel)) {
			continue
		}
		found = append(found, planRiskDeclaration{line: index + 1, text: trimmed, underRisk: riskLevel > 0})
	}
	if len(found) == 0 {
		return "", "", nil
	}
	if len(found) > 1 {
		return "", "", fmt.Errorf("plan risk tier is stated more than once (lines %d and %d); state it once under the Risks heading as %s", found[0].line, found[1].line, PlanRiskTierFormat)
	}
	declaration := found[0]
	if !declaration.underRisk {
		return "", "", fmt.Errorf("plan risk tier on line %d must be under the Risks heading, as %s", declaration.line, PlanRiskTierFormat)
	}
	return parsePlanRiskTierLine(declaration)
}

func parsePlanRiskTierLine(declaration planRiskDeclaration) (riskpolicy.Tier, string, error) {
	malformed := func(detail string) error {
		return fmt.Errorf("plan risk tier on line %d is malformed: %s; write it as %s", declaration.line, detail, PlanRiskTierFormat)
	}
	if !strings.HasPrefix(declaration.text, PlanRiskTierLabel) {
		return "", "", malformed("the line must start with \"Risk tier:\" and carry no list marker, quote, or emphasis")
	}
	rest := strings.TrimSpace(strings.TrimPrefix(declaration.text, PlanRiskTierLabel))
	tierText, reason, hasSeparator := strings.Cut(rest, PlanRiskTierSeparator)
	tierText = strings.TrimSpace(tierText)
	if !hasSeparator {
		return "", "", malformed("the tier and the reason must be separated by an em dash (—)")
	}
	tier := riskpolicy.Tier(tierText)
	if !tier.Valid() {
		return "", "", malformed(fmt.Sprintf("the tier %q must be low, medium, or high in lowercase", tierText))
	}
	reason = strings.TrimSpace(reason)
	if err := core.ValidateRiskReason("the reason", reason); err != nil {
		return "", "", malformed(err.Error())
	}
	return tier, reason, nil
}

// planHeadingName normalizes a heading's text as ParsePlan names headings.
func planHeadingName(text string) string {
	text = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), "#"))
	name := strings.ToLower(text)
	name = strings.ReplaceAll(name, "-", " ")
	return strings.Join(strings.Fields(name), " ")
}

// fenceOpener returns the fence marker a line opens, or "".
func fenceOpener(trimmed string) string {
	for _, char := range []string{"`", "~"} {
		count := 0
		for count < len(trimmed) && trimmed[count] == char[0] {
			count++
		}
		if count >= 3 {
			return strings.Repeat(char, count)
		}
	}
	return ""
}

// closesFence reports whether a line closes a fence opened by marker.
func closesFence(trimmed, marker string) bool {
	if !strings.HasPrefix(trimmed, marker) {
		return false
	}
	return strings.Trim(trimmed, marker[:1]) == ""
}

func leadingSpaces(line string) int {
	count := 0
	for _, char := range line {
		switch char {
		case ' ':
			count++
		case '\t':
			count += 4
		default:
			return count
		}
	}
	return count
}

// CheckPlanRiskTier refuses a plan that states no risk tier when the risk
// policy that governs it is in enforce mode. Under record a missing tier is
// accepted and a stated one is kept; under off, or with no policy, nothing is
// required (req-risk-based-review AC-4.1, AC-4.2, AC-4.5, AC-1.1). The check
// never reads or sets a plan state (AC-2.2, AC-4.6).
func CheckPlanRiskTier(policy *riskpolicy.Policy, plan Spec) error {
	if policy.Enforcing() && plan.RiskTier == "" {
		return ErrPlanRiskTierMissing
	}
	return nil
}

// PlanRiskOf returns the risk tier and reason a stored plan states, read with
// the shared grammar. Content that is not a valid plan, or whose declaration
// is malformed, states no tier, so an earlier version stays readable without
// an invented rating.
func PlanRiskOf(content string) (riskpolicy.Tier, string) {
	if _, err := parsePlanStructure(content, nil); err != nil {
		return "", ""
	}
	tier, reason, err := parsePlanRiskTier(strings.TrimSpace(content))
	if err != nil {
		return "", ""
	}
	return tier, reason
}
