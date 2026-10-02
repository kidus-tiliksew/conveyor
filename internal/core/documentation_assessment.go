package core

import (
	"fmt"
	"sort"
	"strings"
)

// DocumentationAssessment is the reviewer's documentation-closure judgment for
// one reviewed head. Applicable is true exactly when a pinned policy declares
// at least one docs path. The three lists classify disjoint outcomes: the
// declared docs actually updated in the pull request, unresolved documentation
// findings (behavior changed but the declared docs edit or docs-none reason
// does not hold), and disjoint classification conflicts (for example a claimed
// path outside the policy globs).
type DocumentationAssessment struct {
	Applicable   bool     `json:"applicable"`
	Summary      string   `json:"summary"`
	UpdatedPaths []string `json:"updated_paths"`
	Unresolved   []string `json:"unresolved"`
	Conflicts    []string `json:"conflicts"`
}

// NormalizeDocumentationAssessment trims each entry, drops duplicates within a
// list, sorts the lists, and rejects a finding that appears in more than one
// category. Empty entries are preserved so the durable validator can reject
// them with a precise diagnostic instead of silently dropping model output.
func NormalizeDocumentationAssessment(value *DocumentationAssessment) error {
	if value == nil {
		return nil
	}
	value.Summary = strings.TrimSpace(value.Summary)
	lists := []*[]string{&value.UpdatedPaths, &value.Unresolved, &value.Conflicts}
	seen := map[string]int{}
	for i, list := range lists {
		unique := map[string]bool{}
		out := make([]string, 0, len(*list))
		for _, raw := range *list {
			item := strings.TrimSpace(raw)
			if unique[item] {
				continue
			}
			unique[item] = true
			if item != "" {
				if prior, exists := seen[item]; exists && prior != i {
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
