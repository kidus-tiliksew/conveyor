// Package pipeline validates the machine-owned outputs that move a task
// through the factory pipeline (spec §4, §4.1).
package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Triage struct {
	Class          string  `json:"class"`
	Automatability float64 `json:"automatability"`
	Route          string  `json:"route"`
	Summary        string  `json:"summary"`
}

type Review struct {
	Verdict    string `json:"verdict"`
	ReasonCode string `json:"reason_code"`
	Summary    string `json:"summary"`
	Feedback   string `json:"feedback"`
}

type AcceptanceCriterion struct {
	ID        string `yaml:"id" json:"id"`
	Criterion string `yaml:"criterion" json:"criterion"`
	Verify    string `yaml:"verify" json:"verify"`
	Ref       string `yaml:"ref" json:"ref,omitempty"`
}

type DecompositionItem struct {
	ID        string   `yaml:"id" json:"id"`
	Repo      string   `yaml:"repo" json:"repo"`
	Summary   string   `yaml:"summary" json:"summary"`
	DependsOn []string `yaml:"depends_on" json:"depends_on"`
}

type Spec struct {
	Markdown      string
	Acceptance    []AcceptanceCriterion
	Decomposition []DecompositionItem
}

var (
	acceptanceIDPattern    = regexp.MustCompile(`^AC-[1-9][0-9]*$`)
	decompositionIDPattern = regexp.MustCompile(`^SUB-[1-9][0-9]*$`)
)

func ParseTriage(output string) (Triage, error) {
	var value Triage
	if err := parseJSONFence(output, "triage", &value); err != nil {
		return value, err
	}
	if value.Class != "bug" && value.Class != "feature" && value.Class != "chore" {
		return value, fmt.Errorf("triage class must be bug, feature, or chore")
	}
	if value.Automatability < 0 || value.Automatability > 1 {
		return value, fmt.Errorf("triage automatability must be between 0 and 1")
	}
	switch value.Route {
	case "implement", "spec", "human", "parked":
	default:
		return value, fmt.Errorf("triage route must be implement, spec, human, or parked")
	}
	if strings.TrimSpace(value.Summary) == "" {
		return value, fmt.Errorf("triage summary is required")
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
	return value, nil
}

func ParseSpec(output string) (Spec, error) {
	markdown := strings.TrimSpace(output)
	if !strings.Contains(markdown, "## Intent") || !strings.Contains(markdown, "## Non-goals") {
		return Spec{}, fmt.Errorf("spec requires Intent and Non-goals sections")
	}
	acceptanceRaw, ok := fence(markdown, "acceptance")
	if !ok {
		return Spec{}, fmt.Errorf("spec requires one conveyor:acceptance block")
	}
	var acceptance []AcceptanceCriterion
	if err := yaml.Unmarshal([]byte(acceptanceRaw), &acceptance); err != nil {
		return Spec{}, fmt.Errorf("acceptance block: %w", err)
	}
	if len(acceptance) == 0 {
		return Spec{}, fmt.Errorf("acceptance block must not be empty")
	}
	seen := map[string]bool{}
	for _, criterion := range acceptance {
		if !acceptanceIDPattern.MatchString(criterion.ID) || seen[criterion.ID] {
			return Spec{}, fmt.Errorf("acceptance IDs must be unique AC-n values")
		}
		seen[criterion.ID] = true
		if strings.TrimSpace(criterion.Criterion) == "" {
			return Spec{}, fmt.Errorf("acceptance criterion %s is empty", criterion.ID)
		}
		switch criterion.Verify {
		case "test", "playwright", "computer-use", "human":
		default:
			return Spec{}, fmt.Errorf("acceptance criterion %s has invalid verify method", criterion.ID)
		}
	}
	decomposition := []DecompositionItem{}
	if raw, exists := fence(markdown, "decomposition"); exists {
		if err := yaml.Unmarshal([]byte(raw), &decomposition); err != nil {
			return Spec{}, fmt.Errorf("decomposition block: %w", err)
		}
		ids := map[string]bool{}
		for _, item := range decomposition {
			if !decompositionIDPattern.MatchString(item.ID) || ids[item.ID] || item.Repo == "" || item.Summary == "" {
				return Spec{}, fmt.Errorf("decomposition items require unique SUB-n IDs, repo, and summary")
			}
			ids[item.ID] = true
		}
		for _, item := range decomposition {
			for _, dependency := range item.DependsOn {
				if !ids[dependency] {
					return Spec{}, fmt.Errorf("decomposition %s depends on unknown %s", item.ID, dependency)
				}
			}
		}
	}
	return Spec{Markdown: markdown, Acceptance: acceptance, Decomposition: decomposition}, nil
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
