// Package pack loads the reviewable role prompts used by in-process stages
// and MCP work-order context. Sandbox tool policies retired in Phase 4.7
// (spec §21.4).
package pack

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Loader struct{ Dir string }

var pipelineStages = []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageReview}

type Bundle struct {
	roles        map[core.Stage]string
	planningRole string
}

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
	planningRole, err := (Loader{Dir: dir}).PlanningRole()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace([]byte(planningRole))) == 0 {
		return nil, fmt.Errorf("load planning role prompt: file is empty")
	}
	bundle.planningRole = planningRole
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

func (b *Bundle) PlanningRole() (string, error) {
	if b == nil {
		return "", fmt.Errorf("prompt pack is not loaded")
	}
	if strings.TrimSpace(b.planningRole) == "" {
		return "", fmt.Errorf("pack has no planning role")
	}
	return b.planningRole, nil
}

func (l Loader) Role(stage core.Stage) (string, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, "roles", string(stage)+".md"))
	if err != nil {
		return "", fmt.Errorf("load %s role prompt: %w", stage, err)
	}
	return string(data), nil
}

func (l Loader) PlanningRole() (string, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, "roles", "planning.md"))
	if err != nil {
		return "", fmt.Errorf("load planning role prompt: %w", err)
	}
	return string(data), nil
}

// InProcessReviewRole adds the execution environment and the structured
// output contract consumed by pipeline.ParseReview. The in-process Responses
// API call has no Conveyor MCP tools, no checkout, and no filesystem.
func InProcessReviewRole(role string) string {
	return strings.TrimSpace(role) + `

This review is a single in-process model call: you have no tools, no
repository checkout, and no way to open files — the branch diff under
review and its context are supplied in this prompt. Do not announce plans
to inspect code or ask for more material; judge from what is provided and
record anything you could not verify in the summary. Your one and only
response must contain the verdict.

End your answer with exactly one machine-owned block and nothing after it:

` + "```conveyor:review\n" + `{"verdict":"approve|changes_requested","reason_code":"approved|scope-creep|hallucinated-API|style|flaky-env|other","summary":"concise assessment citing AC-n status","feedback":"specific implementation guidance, empty only on approval","requirement_citations":{"applicable":true,"cited_ids":[],"unknown_ids":[],"unserved_ids":[],"conflicts":[]}}
` + "```"
}

// WithRequirementCitationContract binds implementation and review guidance to
// the confirmed served requirements reached through canonical lineage. Review
// findings are structured evidence, not a source-code parser (spec §4.2 item
// 4).
func WithRequirementCitationContract(role string, stage core.Stage, requirements []core.ServedRequirementContext) string {
	var contract strings.Builder
	contract.WriteString(strings.TrimSpace(role))
	if len(requirements) == 0 {
		if stage == core.StageReview {
			contract.WriteString("\n\n# Requirement citation contract\n\nNo confirmed served requirement is linked to this task. Record requirement_citations with applicable=false and all four finding lists empty; an unlinked task remains legal.\n")
		}
		return contract.String()
	}
	contract.WriteString("\n\n# Confirmed served requirements\n\n")
	for _, requirement := range requirements {
		fmt.Fprintf(&contract, "- %s v%d — %s\n", requirement.ID, requirement.Version, requirement.Title)
		for _, statement := range requirement.Statements {
			fmt.Fprintf(&contract, "  - %s: %s\n", statement.ID, statement.Statement)
		}
	}
	if stage == core.StageImplement {
		contract.WriteString("\nFor implementation decisions governed by these statements, cite the applicable stable REQ-n IDs in code comments alongside existing (spec §N) citations. Do not add ornamental citations where no implementation decision needs explanation.\n")
	}
	if stage == core.StageReview {
		contract.WriteString("\nValidate REQ-n citations against the confirmed statements above and the approved governing spec. Record requirement_citations with applicable=true plus cited_ids, unknown_ids, unserved_ids, and conflicts arrays. This is a reasoned review assessment, not a claim of exhaustive source parsing.\n")
	}
	return contract.String()
}

// MCPReviewRole adds the terminal lifecycle contract used by operator-owned
// Codex and Claude reviewers. Their prose or JSON output is never a verdict.
func MCPReviewRole(role string) string {
	return strings.TrimSpace(role) + `

You are running in a read-only checkout on the task branch. Review the
branch diff against its base; you may read any file for context, but judge
only what the diff changes.

Before ending, call Conveyor's ` + "`submit_review_verdict`" + ` MCP tool with
your verdict, reason code, summary, feedback, and requirement-citation
assessment, then wait for and observe a
successful tool response. Printing, returning, or describing verdict JSON is
not completion and is never a substitute for the tool call. A missing or failed
tool response is not terminal success: keep the review active and retry or
report the tool failure instead of claiming that the verdict was submitted.`
}
