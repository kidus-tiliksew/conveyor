package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DocumentationAssessment records review judgment for exact-head documentation evidence.
type DocumentationAssessment struct {
	Applicable   bool     `json:"applicable"`
	Summary      string   `json:"summary"`
	UpdatedPaths []string `json:"updated_paths"`
	Unresolved   []string `json:"unresolved"`
	Conflicts    []string `json:"conflicts"`
}

func NormalizeDocumentationAssessment(value *DocumentationAssessment) error {
	if value == nil {
		return nil
	}
	value.Summary = strings.TrimSpace(value.Summary)
	seen := map[string]int{}
	for i, list := range []*[]string{&value.UpdatedPaths, &value.Unresolved, &value.Conflicts} {
		unique := map[string]bool{}
		out := make([]string, 0, len(*list))
		for _, raw := range *list {
			item := strings.TrimSpace(raw)
			if unique[item] {
				continue
			}
			unique[item] = true
			if item != "" {
				if prior, ok := seen[item]; ok && prior != i {
					return fmt.Errorf("documentation assessment finding %q appears in more than one category", item)
				}
				seen[item] = i
			}
			out = append(out, item)
		}
		sort.Strings(out)
		*list = out
	}
	return nil
}

// DocumentationGateEvidence is the admission evidence for a task and submitted head (DEC-71).
type DocumentationGateEvidence struct {
	TaskID            string          `json:"task_id"`
	HeadSHA           string          `json:"head_sha"`
	BaseSHA           string          `json:"base_sha"`
	PolicyJSON        json.RawMessage `json:"policy_json"`
	ChangedPaths      []string        `json:"changed_paths"`
	MatchedPaths      []string        `json:"matched_paths"`
	AffectedDocuments []string        `json:"affected_documents"`
	AgentAuthoredBody string          `json:"agent_authored_body"`
	NoneStatement     string          `json:"none_statement"`
	NoneReason        string          `json:"none_reason"`
}
