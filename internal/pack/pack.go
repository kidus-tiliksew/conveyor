// Package pack loads Phase 3's reviewable proto-pack role prompts and stage
// tool policies from files (spec §2.2).
package pack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Loader struct{ Dir string }

var pipelineStages = []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageReview}

// Bundle is the boot-validated, immutable proto-pack used for every dispatch.
// Loading once prevents a task from discovering a broken role or policy only
// after it has already crossed earlier pipeline gates.
type Bundle struct {
	roles    map[core.Stage]string
	policies map[core.Stage]adapter.ToolPolicy
}

func Load(dir string) (*Bundle, error) {
	if dir == "" {
		return nil, fmt.Errorf("pack_dir is required for Phase 3 tasks")
	}
	bundle := &Bundle{roles: make(map[core.Stage]string), policies: make(map[core.Stage]adapter.ToolPolicy)}
	for _, stage := range pipelineStages {
		role, err := (Loader{Dir: dir}).Role(stage)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace([]byte(role))) == 0 {
			return nil, fmt.Errorf("load %s role prompt: file is empty", stage)
		}
		bundle.roles[stage] = role

		data, err := os.ReadFile(filepath.Join(dir, "policies", string(stage)+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load %s tool policy: %w", stage, err)
		}
		var policy adapter.ToolPolicy
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			return nil, fmt.Errorf("load %s tool policy: %w", stage, err)
		}
		bundle.policies[stage] = policy
	}
	return bundle, nil
}

func (b *Bundle) Role(stage core.Stage) (string, error) {
	if b == nil {
		return "", fmt.Errorf("Phase 3 pack is not loaded")
	}
	role, ok := b.roles[stage]
	if !ok {
		return "", fmt.Errorf("pack has no %s role", stage)
	}
	return role, nil
}

func (b *Bundle) Policy(stage core.Stage, repo adapter.ToolPolicy) adapter.ToolPolicy {
	if b == nil {
		return repo
	}
	stagePolicy := b.policies[stage]
	merged := adapter.ToolPolicy{
		AllowedCommands: append([][]string(nil), repo.AllowedCommands...),
		DeniedCommands:  append([][]string(nil), repo.DeniedCommands...),
		NetworkAllow:    append([]string(nil), repo.NetworkAllow...),
	}
	merged.AllowedCommands = append(merged.AllowedCommands, stagePolicy.AllowedCommands...)
	merged.DeniedCommands = append(merged.DeniedCommands, stagePolicy.DeniedCommands...)
	return merged
}

func (l Loader) Role(stage core.Stage) (string, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, "roles", string(stage)+".md"))
	if err != nil {
		return "", fmt.Errorf("load %s role prompt: %w", stage, err)
	}
	return string(data), nil
}

func (l Loader) Policy(stage core.Stage, repo adapter.ToolPolicy) (adapter.ToolPolicy, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, "policies", string(stage)+".json"))
	if os.IsNotExist(err) {
		return repo, nil
	}
	if err != nil {
		return adapter.ToolPolicy{}, err
	}
	var stagePolicy adapter.ToolPolicy
	if err := json.Unmarshal(data, &stagePolicy); err != nil {
		return adapter.ToolPolicy{}, fmt.Errorf("load %s tool policy: %w", stage, err)
	}
	// Denies accumulate. Allows are advisory permits in both adapters, so the
	// repo and role permits may safely coexist while deny continues to win.
	repo.AllowedCommands = append(repo.AllowedCommands, stagePolicy.AllowedCommands...)
	repo.DeniedCommands = append(repo.DeniedCommands, stagePolicy.DeniedCommands...)
	return repo, nil
}
