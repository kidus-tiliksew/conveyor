// Package pack loads the reviewable role prompts used by in-process stages
// and MCP work-order context. Sandbox tool policies retired in Phase 4.7
// (spec §21.4).
package pack

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Loader struct{ Dir string }

var pipelineStages = []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageReview}

type Bundle struct{ roles map[core.Stage]string }

func Load(dir string) (*Bundle, error) {
	if dir == "" {
		return nil, fmt.Errorf("pack_dir is required")
	}
	bundle := &Bundle{roles: make(map[core.Stage]string)}
	for _, stage := range pipelineStages {
		role, err := (Loader{Dir: dir}).Role(stage)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace([]byte(role))) == 0 {
			return nil, fmt.Errorf("load %s role prompt: file is empty", stage)
		}
		bundle.roles[stage] = role
	}
	return bundle, nil
}

func (b *Bundle) Role(stage core.Stage) (string, error) {
	if b == nil {
		return "", fmt.Errorf("prompt pack is not loaded")
	}
	role, ok := b.roles[stage]
	if !ok {
		return "", fmt.Errorf("pack has no %s role", stage)
	}
	return role, nil
}

func (l Loader) Role(stage core.Stage) (string, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, "roles", string(stage)+".md"))
	if err != nil {
		return "", fmt.Errorf("load %s role prompt: %w", stage, err)
	}
	return string(data), nil
}
