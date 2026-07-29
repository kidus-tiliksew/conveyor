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

// ValidateBlueprintDecomposition enforces the canonical decomposition schema
// and rejects duplicate, dangling, or cyclic dependency graphs (spec §4.1).
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
