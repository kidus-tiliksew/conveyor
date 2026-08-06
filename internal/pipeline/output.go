// Package pipeline validates the machine-owned outputs that move a task
// through the factory pipeline (spec §4, §4.1).
package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"gopkg.in/yaml.v3"
)

type Triage struct {
	Class   string      `json:"class"`
	Route   string      `json:"route"`
	Summary string      `json:"summary"`
	Brief   TriageBrief `json:"brief"`
	// RequirementID proposes a requirement relation for a stray task. It
	// replaces the retired feature suggestion (spec §4.2 item 1, §21.46
	// change 5): triage proposes, an operator confirms.
	RequirementID string `json:"requirement_id,omitempty"`
}

// TriageBrief frames downstream investigation without becoming normative.
type TriageBrief struct {
	Questions     []string `json:"questions"`
	AffectedAreas []string `json:"affected_areas"`
	Risks         []string `json:"risks"`
}

type Review struct {
	Verdict              string                              `json:"verdict"`
	ReasonCode           string                              `json:"reason_code"`
	Summary              string                              `json:"summary"`
	Feedback             string                              `json:"feedback"`
	RequirementCitations *core.RequirementCitationAssessment `json:"requirement_citations,omitempty"`
	DoneCriteriaCoverage *core.DoneCriteriaAssessment        `json:"done_criteria_coverage,omitempty"`
	GovernanceAssessment *core.GovernanceAssessment          `json:"governance_assessment,omitempty"`
}

type AcceptanceCriterion struct {
	ID        string `yaml:"id" json:"id"`
	Criterion string `yaml:"criterion" json:"criterion"`
	Verify    string `yaml:"verify" json:"verify"`
	Ref       string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

type DecompositionItem = core.BlueprintDecompositionItem

type Spec struct {
	Markdown      string
	Acceptance    []AcceptanceCriterion
	Decomposition []DecompositionItem
}

// StructuredSpec is the model-owned semantic spec result. Conveyor validates
// this value and owns the canonical machine-block serialization (spec §4.1).
type StructuredSpec struct {
	Markdown      string                `json:"markdown"`
	Acceptance    []AcceptanceCriterion `json:"acceptance"`
	Decomposition []DecompositionItem   `json:"decomposition"`
}

// StructuredPlan is the task-scoped execution plan submitted through the
// re-contented spec-stage machinery. Decomposition stays in the wire shape so
// an attempted fan-out receives a precise in-band validation error.
type StructuredPlan struct {
	Markdown      string              `json:"markdown"`
	Decomposition []DecompositionItem `json:"decomposition"`
}

var (
	acceptanceIDPattern = regexp.MustCompile(`^AC-[1-9][0-9]*$`)
)

func ParseTriage(output string) (Triage, error) {
	var value Triage
	if err := parseJSONFence(output, "triage", &value); err != nil {
		return value, err
	}
	if value.Class != "bug" && value.Class != "feature" && value.Class != "chore" {
		return value, fmt.Errorf("triage class must be bug, feature, or chore")
	}
	switch value.Route {
	case "implement", "spec", "human", "parked":
	default:
		return value, fmt.Errorf("triage route must be implement, spec, human, or parked")
	}
	if strings.TrimSpace(value.Summary) == "" {
		return value, fmt.Errorf("triage summary is required")
	}
	if value.Brief.Questions == nil {
		value.Brief.Questions = []string{}
	}
	if value.Brief.AffectedAreas == nil {
		value.Brief.AffectedAreas = []string{}
	}
	if value.Brief.Risks == nil {
		value.Brief.Risks = []string{}
	}
	return value, nil
}

func ParseReview(output string) (Review, error) {
	var value Review
	if err := parseJSONFence(output, "review", &value); err != nil {
		return value, err
	}
	if value.Verdict != "approve" && value.Verdict != "changes_requested" {
		return value, fmt.Errorf("review verdict must be approve or changes_requested")
	}
	if strings.TrimSpace(value.ReasonCode) == "" || strings.TrimSpace(value.Summary) == "" {
		return value, fmt.Errorf("review reason_code and summary are required")
	}
	if value.Verdict == "changes_requested" && strings.TrimSpace(value.Feedback) == "" {
		return value, fmt.Errorf("review feedback is required when changes are requested")
	}
	if value.RequirementCitations != nil {
		value.RequirementCitations.CitedIDs = nonNilStrings(value.RequirementCitations.CitedIDs)
		value.RequirementCitations.UnknownIDs = nonNilStrings(value.RequirementCitations.UnknownIDs)
		value.RequirementCitations.UnservedIDs = nonNilStrings(value.RequirementCitations.UnservedIDs)
		value.RequirementCitations.Conflicts = nonNilStrings(value.RequirementCitations.Conflicts)
	}
	if value.DoneCriteriaCoverage != nil {
		value.DoneCriteriaCoverage.Satisfied = nonNilStrings(value.DoneCriteriaCoverage.Satisfied)
		value.DoneCriteriaCoverage.Unsatisfied = nonNilStrings(value.DoneCriteriaCoverage.Unsatisfied)
		value.DoneCriteriaCoverage.Unverified = nonNilStrings(value.DoneCriteriaCoverage.Unverified)
		value.DoneCriteriaCoverage.Conflicts = nonNilStrings(value.DoneCriteriaCoverage.Conflicts)
	}
	if value.GovernanceAssessment != nil {
		if err := core.NormalizeGovernanceAssessment(value.GovernanceAssessment); err != nil {
			return value, err
		}
	}
	return value, nil
}

var planHeadingPattern = regexp.MustCompile(`(?im)^#{1,6}[ \t]+([^\r\n#]+?)[ \t]*#*[ \t]*$`)

// ParsePlan validates a Markdown execution plan without introducing a second
// document lifecycle. Plans are stored as ordinary spec versions until the
// later retirement slice renames the persistence model (spec §21.58 change 4).
func ParsePlan(markdown string, decomposition []DecompositionItem) (Spec, error) {
	markdown = strings.TrimSpace(markdown)
	if markdown == "" {
		return Spec{}, fmt.Errorf("plan markdown is required")
	}
	if len(decomposition) != 0 {
		return Spec{}, fmt.Errorf("plans cannot contain a decomposition; report oversized scope through progress/check-in instead")
	}
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") && strings.Contains(strings.ToLower(trimmed), "conveyor:") {
			return Spec{}, fmt.Errorf("plans cannot contain conveyor: machine fences")
		}
	}
	headings := map[string]bool{}
	for _, match := range planHeadingPattern.FindAllStringSubmatch(markdown, -1) {
		name := strings.ToLower(strings.TrimSpace(match[1]))
		name = strings.ReplaceAll(name, "-", " ")
		name = strings.Join(strings.Fields(name), " ")
		switch name {
		case "approach":
			headings["approach"] = true
		case "files touched", "files to touch":
			headings["files"] = true
		case "ordering":
			headings["ordering"] = true
		case "risks":
			headings["risks"] = true
		case "done criteria", "definition of done":
			headings["done criteria"] = true
		}
	}
	for _, required := range []string{"approach", "files", "ordering", "risks", "done criteria"} {
		if !headings[required] {
			return Spec{}, fmt.Errorf("plan requires a recognizable %s heading", required)
		}
	}
	return Spec{Markdown: markdown, Acceptance: []AcceptanceCriterion{}, Decomposition: []DecompositionItem{}}, nil
}

// StructuredPlanSchema is the only model-output contract for newly dispatched
// plan-stage work after the Phase 8.3 retirement flip.
func StructuredPlanSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"markdown": map[string]any{"type": "string", "minLength": 1},
			"decomposition": map[string]any{
				"type": "array", "maxItems": 0,
				"items": map[string]any{"type": "object"},
			},
		},
		"required": []string{"markdown", "decomposition"},
	}
}

func RenderStructuredPlan(output string) (Spec, error) {
	var value StructuredPlan
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Spec{}, fmt.Errorf("structured plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Spec{}, fmt.Errorf("structured plan contains more than one JSON value")
		}
		return Spec{}, fmt.Errorf("structured plan has trailing data: %w", err)
	}
	return ParsePlan(value.Markdown, value.Decomposition)
}

// PlanDoneCriteria returns the done-criteria section, including its heading,
// for review prompt rendering. The same heading rules as ParsePlan apply.
func PlanDoneCriteria(markdown string) (string, bool) {
	matches := planHeadingPattern.FindAllStringSubmatchIndex(markdown, -1)
	for i, match := range matches {
		name := strings.ToLower(strings.TrimSpace(markdown[match[2]:match[3]]))
		name = strings.Join(strings.Fields(strings.ReplaceAll(name, "-", " ")), " ")
		if name != "done criteria" && name != "definition of done" {
			continue
		}
		end := len(markdown)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		return strings.TrimSpace(markdown[match[0]:end]), true
	}
	return "", false
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func ParseSpec(output string) (Spec, error) {
	markdown := strings.TrimSpace(output)
	if !strings.Contains(markdown, "## Intent") || !strings.Contains(markdown, "## Non-goals") {
		return Spec{}, fmt.Errorf("spec requires Intent and Non-goals sections")
	}
	acceptanceBlocks := fences(markdown, "acceptance")
	if len(acceptanceBlocks) == 0 {
		return Spec{}, missingSpecFenceError(markdown, "acceptance")
	}
	if len(acceptanceBlocks) != 1 {
		return Spec{}, fmt.Errorf("spec requires exactly one conveyor:acceptance block; found %d", len(acceptanceBlocks))
	}
	var acceptance []AcceptanceCriterion
	if err := decodeYAMLList(acceptanceBlocks[0], &acceptance); err != nil {
		return Spec{}, fmt.Errorf("acceptance block: %w", err)
	}
	if err := validateAcceptance(acceptance); err != nil {
		return Spec{}, err
	}
	decomposition := []DecompositionItem{}
	decompositionBlocks := fences(markdown, "decomposition")
	if len(decompositionBlocks) > 1 {
		return Spec{}, fmt.Errorf("spec permits at most one conveyor:decomposition block; found %d", len(decompositionBlocks))
	}
	if len(decompositionBlocks) == 1 {
		if err := decodeYAMLList(decompositionBlocks[0], &decomposition); err != nil {
			return Spec{}, fmt.Errorf("decomposition block: %w", err)
		}
		if err := core.ValidateBlueprintDecomposition(decomposition); err != nil {
			return Spec{}, err
		}
	}
	return Spec{Markdown: markdown, Acceptance: acceptance, Decomposition: decomposition}, nil
}

// StructuredSpecSchema is the strict Responses-compatible contract used only
// for in-process specification generation. Optional decomposition is encoded
// as an empty list; an optional acceptance ref is encoded as string or null.
func StructuredSpecSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"markdown": map[string]any{"type": "string", "minLength": 1},
			"acceptance": map[string]any{
				"type": "array", "minItems": 1,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"id":        map[string]any{"type": "string", "pattern": `^AC-[1-9][0-9]*$`},
						"criterion": map[string]any{"type": "string", "minLength": 1},
						"verify":    map[string]any{"type": "string", "enum": []string{"test", "playwright", "computer-use", "human"}},
						"ref":       map[string]any{"type": []string{"string", "null"}},
					},
					"required": []string{"id", "criterion", "verify", "ref"},
				},
			},
			"decomposition": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"id":         map[string]any{"type": "string", "pattern": `^SUB-[1-9][0-9]*$`},
						"repo":       map[string]any{"type": "string", "minLength": 1},
						"summary":    map[string]any{"type": "string", "minLength": 1},
						"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string", "pattern": `^SUB-[1-9][0-9]*$`}},
					},
					"required": []string{"id", "repo", "summary", "depends_on"},
				},
			},
		},
		"required": []string{"markdown", "acceptance", "decomposition"},
	}
}

// RenderStructuredSpec validates typed model output, renders exact fenced YAML
// blocks, and then re-parses the document through ParseSpec as the final
// invariant. Model-authored machine fences are rejected rather than repaired.
func RenderStructuredSpec(output string) (Spec, error) {
	var value StructuredSpec
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Spec{}, fmt.Errorf("structured spec output must match the required object shape: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Spec{}, fmt.Errorf("structured spec output contains more than one JSON value")
		}
		return Spec{}, fmt.Errorf("structured spec output has trailing data: %w", err)
	}
	markdown := strings.TrimSpace(value.Markdown)
	if !strings.Contains(markdown, "## Intent") || !strings.Contains(markdown, "## Non-goals") {
		return Spec{}, fmt.Errorf("structured spec markdown requires Intent and Non-goals sections")
	}
	if machineFenceNearMiss(markdown, "acceptance") != "" || machineFenceNearMiss(markdown, "decomposition") != "" {
		return Spec{}, fmt.Errorf("structured spec markdown must not contain model-authored conveyor machine fences; Conveyor serializes them")
	}
	if err := validateAcceptance(value.Acceptance); err != nil {
		return Spec{}, err
	}
	if err := core.ValidateBlueprintDecomposition(value.Decomposition); err != nil {
		return Spec{}, err
	}
	acceptanceYAML, err := yaml.Marshal(value.Acceptance)
	if err != nil {
		return Spec{}, fmt.Errorf("render acceptance block: %w", err)
	}
	var rendered strings.Builder
	rendered.WriteString(markdown)
	rendered.WriteString("\n\n```conveyor:acceptance\n")
	rendered.Write(acceptanceYAML)
	rendered.WriteString("```")
	if len(value.Decomposition) != 0 {
		decompositionYAML, marshalErr := yaml.Marshal(value.Decomposition)
		if marshalErr != nil {
			return Spec{}, fmt.Errorf("render decomposition block: %w", marshalErr)
		}
		rendered.WriteString("\n\n```conveyor:decomposition\n")
		rendered.Write(decompositionYAML)
		rendered.WriteString("```")
	}
	parsed, err := ParseSpec(rendered.String())
	if err != nil {
		return Spec{}, fmt.Errorf("Conveyor-rendered spec failed canonical ParseSpec validation: %w", err)
	}
	return parsed, nil
}

func validateAcceptance(acceptance []AcceptanceCriterion) error {
	if len(acceptance) == 0 {
		return fmt.Errorf("acceptance must be a non-empty list")
	}
	seen := map[string]bool{}
	for index, criterion := range acceptance {
		if !acceptanceIDPattern.MatchString(criterion.ID) {
			return fmt.Errorf("acceptance item %d has invalid id %q; want AC-n", index+1, criterion.ID)
		}
		if seen[criterion.ID] {
			return fmt.Errorf("acceptance contains duplicate id %q", criterion.ID)
		}
		seen[criterion.ID] = true
		if strings.TrimSpace(criterion.Criterion) == "" {
			return fmt.Errorf("acceptance criterion %s has an empty criterion", criterion.ID)
		}
		switch criterion.Verify {
		case "test", "playwright", "computer-use", "human":
		default:
			return fmt.Errorf("acceptance criterion %s has invalid verify value %q; want test, playwright, computer-use, or human", criterion.ID, criterion.Verify)
		}
	}
	return nil
}

func decodeYAMLList(raw string, destination any) error {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.SequenceNode {
		if len(document.Content) != 0 && document.Content[0].Kind == yaml.MappingNode && len(document.Content[0].Content) != 0 {
			return fmt.Errorf("must be a top-level YAML list; wrapper key %q is not allowed", document.Content[0].Content[0].Value)
		}
		return fmt.Errorf("must be a top-level YAML list")
	}
	if err := document.Content[0].Decode(destination); err != nil {
		return err
	}
	return nil
}

func missingSpecFenceError(markdown, kind string) error {
	if opening := machineFenceNearMiss(markdown, kind); opening != "" {
		return fmt.Errorf("spec requires exact ```conveyor:%s fence; rejected near-miss opening %q", kind, opening)
	}
	return fmt.Errorf("spec requires one conveyor:%s block", kind)
}

func machineFenceNearMiss(output, kind string) string {
	needle := "conveyor:" + kind
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") && strings.Contains(trimmed, needle) {
			return trimmed
		}
	}
	return ""
}

func parseJSONFence(output, kind string, destination any) error {
	raw, ok := fence(output, kind)
	if !ok {
		return fmt.Errorf("missing conveyor:%s fenced block", kind)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("conveyor:%s block: %w", kind, err)
	}
	return nil
}

func fence(output, kind string) (string, bool) {
	marker := "```conveyor:" + kind
	start := strings.Index(output, marker)
	if start < 0 {
		return "", false
	}
	start += len(marker)
	if newline := strings.IndexByte(output[start:], '\n'); newline >= 0 {
		start += newline + 1
	} else {
		return "", false
	}
	end := strings.Index(output[start:], "```")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(output[start : start+end]), true
}

func fences(output, kind string) []string {
	marker := "```conveyor:" + kind
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	blocks := []string{}
	for index := 0; index < len(lines); index++ {
		if lines[index] != marker {
			continue
		}
		start := index + 1
		for index = start; index < len(lines); index++ {
			if lines[index] == "```" {
				blocks = append(blocks, strings.TrimSpace(strings.Join(lines[start:index], "\n")))
				break
			}
		}
	}
	return blocks
}
