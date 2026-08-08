package core

import (
	"fmt"
	"regexp"
	"strings"
)

var blueprintDecompositionIDPattern = regexp.MustCompile(`^SUB-[1-9][0-9]*$`)

// BlueprintDecompositionItem is the canonical §4.1 decomposition fence item.
// Store materialization and pipeline parsing deliberately share this type and
// validator so their accepted graph shapes cannot drift.
type BlueprintDecompositionItem struct {
	ID        string   `yaml:"id" json:"id"`
	Repo      string   `yaml:"repo" json:"repo"`
	Summary   string   `yaml:"summary" json:"summary"`
	DependsOn []string `yaml:"depends_on" json:"depends_on"`
}

// BlueprintAnchor reports whether a task is a blueprint anchor:
// an intent artifact rather than work. Children are created only by blueprint
// materialization, which requires an approved spec carrying a non-empty §4.1
// decomposition, so the parent/child relation is that decomposition made
// durable. Classifying from it keeps the anchor a derived predicate over
// existing relations — no stored flag, no epic entity (§21.46 change 10).
func BlueprintAnchor(task Task) bool { return len(task.Children) > 0 }

// OrderDecompositionByDependency returns the decomposition in dependency
// order: an item never precedes anything it depends on, and independent items
// keep their stored order. The graph is validated acyclic at materialization
// (ValidateBlueprintDecomposition), so this is a total order; any item left
// unreachable by a malformed graph is appended rather than dropped.
func OrderDecompositionByDependency(items []BlueprintDecompositionItem) []BlueprintDecompositionItem {
	byID := make(map[string]BlueprintDecompositionItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	ordered := make([]BlueprintDecompositionItem, 0, len(items))
	placed := make(map[string]bool, len(items))
	visiting := make(map[string]bool, len(items))
	var visit func(BlueprintDecompositionItem)
	visit = func(item BlueprintDecompositionItem) {
		if placed[item.ID] || visiting[item.ID] {
			return
		}
		visiting[item.ID] = true
		for _, dependency := range item.DependsOn {
			if next, exists := byID[dependency]; exists {
				visit(next)
			}
		}
		delete(visiting, item.ID)
		placed[item.ID] = true
		ordered = append(ordered, item)
	}
	for _, item := range items {
		visit(item)
	}
	return ordered
}

// ValidateBlueprintDecomposition enforces the canonical decomposition schema
// and rejects duplicate, dangling, or cyclic dependency graphs.
func ValidateBlueprintDecomposition(items []BlueprintDecompositionItem) error {
	byID := make(map[string]BlueprintDecompositionItem, len(items))
	for index, item := range items {
		if !blueprintDecompositionIDPattern.MatchString(item.ID) {
			return fmt.Errorf("decomposition item %d has invalid id %q; want SUB-n", index+1, item.ID)
		}
		if _, exists := byID[item.ID]; exists {
			return fmt.Errorf("decomposition contains duplicate id %q", item.ID)
		}
		if strings.TrimSpace(item.Repo) == "" {
			return fmt.Errorf("decomposition %s has an empty repo", item.ID)
		}
		if strings.TrimSpace(item.Summary) == "" {
			return fmt.Errorf("decomposition %s has an empty summary", item.ID)
		}
		byID[item.ID] = item
	}
	for _, item := range items {
		for _, dependency := range item.DependsOn {
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("decomposition %s depends on unknown %s", item.ID, dependency)
			}
		}
	}
	visiting := make(map[string]bool, len(items))
	visited := make(map[string]bool, len(items))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("decomposition dependency cycle includes %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
