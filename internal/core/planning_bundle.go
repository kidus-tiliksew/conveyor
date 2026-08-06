package core

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

type PlanningBundleStatus string

const (
	PlanningBundlePending  PlanningBundleStatus = "pending"
	PlanningBundleApproved PlanningBundleStatus = "approved"
	PlanningBundleRejected PlanningBundleStatus = "rejected"
)

type PlanningBundleDocumentKind string

const (
	PlanningBundleRequirement  PlanningBundleDocumentKind = "requirement"
	PlanningBundleSystemDesign PlanningBundleDocumentKind = "system_design"
	PlanningBundleDecision     PlanningBundleDocumentKind = "decision"
)

type PlanningBundleDocument struct {
	Kind    PlanningBundleDocumentKind `json:"kind"`
	ID      string                     `json:"id"`
	Version int                        `json:"version,omitempty"`
	Title   string                     `json:"title,omitempty"`
	Status  string                     `json:"status,omitempty"`
}

type PlanningBundleTaskContext struct {
	RequirementIDs []string `json:"requirement_ids,omitempty"`
	DesignIDs      []string `json:"system_design_ids,omitempty"`
}

type PlanningBundleTask struct {
	MemberID      string                    `json:"member_id"`
	CreatedTaskID string                    `json:"created_task_id"`
	Title         string                    `json:"title"`
	Body          string                    `json:"body"`
	Repo          string                    `json:"repo"`
	BaseBranch    string                    `json:"base_branch,omitempty"`
	DependsOn     []string                  `json:"depends_on,omitempty"`
	Context       PlanningBundleTaskContext `json:"context,omitempty"`
	SpecApproval  bool                      `json:"spec_approval"`
	MergeApproval bool                      `json:"merge_approval"`
	SetupName     string                    `json:"setup,omitempty"`
	SetupContract config.ExecutionSetup     `json:"setup_contract,omitempty"`
}

// PlanningBundle is one immutable operator preview. Approving it creates its
// task set but deliberately does not confirm any referenced document version.
type PlanningBundle struct {
	ID        string                   `json:"id"`
	SessionID string                   `json:"session_id"`
	Title     string                   `json:"title"`
	Documents []PlanningBundleDocument `json:"documents"`
	Tasks     []PlanningBundleTask     `json:"tasks"`
	Status    PlanningBundleStatus     `json:"status"`
	Workspace string                   `json:"workspace"`
	CreatedBy string                   `json:"created_by,omitempty"`
	DecidedBy string                   `json:"decided_by,omitempty"`
	CreatedAt time.Time                `json:"created_at"`
	DecidedAt time.Time                `json:"decided_at,omitempty"`
}
