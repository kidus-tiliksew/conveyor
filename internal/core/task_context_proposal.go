package core

import (
	"strings"
	"time"
)

// TaskContextProposalState is the operator-owned lifecycle for suggested task
// authority. Proposal and dismissal records grant no authority; confirmation
// is materialized through the ordinary task-context attachment event stream.
type TaskContextProposalState string

const (
	TaskContextProposalProposed  TaskContextProposalState = "proposed"
	TaskContextProposalConfirmed TaskContextProposalState = "confirmed"
	TaskContextProposalDismissed TaskContextProposalState = "dismissed"
)

func (s TaskContextProposalState) Valid() bool {
	return s == TaskContextProposalProposed || s == TaskContextProposalConfirmed || s == TaskContextProposalDismissed
}

type TaskContextProposalTargetKind string

const (
	TaskContextProposalRequirement  TaskContextProposalTargetKind = "requirement"
	TaskContextProposalSystemDesign TaskContextProposalTargetKind = "system_design"
)

func (k TaskContextProposalTargetKind) Valid() bool {
	return k == TaskContextProposalRequirement || k == TaskContextProposalSystemDesign
}

type TaskContextProposalSource string

const (
	TaskContextProposalPlanning TaskContextProposalSource = "planning"
	TaskContextProposalTriage   TaskContextProposalSource = "triage"
	TaskContextProposalOperator TaskContextProposalSource = "operator"
)

func (s TaskContextProposalSource) Valid() bool {
	return s == TaskContextProposalPlanning || s == TaskContextProposalTriage || s == TaskContextProposalOperator
}

type TaskContextProposal struct {
	TaskID           string                        `json:"task_id"`
	TargetKind       TaskContextProposalTargetKind `json:"target_kind"`
	TargetID         string                        `json:"target_id"`
	TargetTitle      string                        `json:"target_title"`
	State            TaskContextProposalState      `json:"state"`
	Source           TaskContextProposalSource     `json:"source"`
	Justification    string                        `json:"justification"`
	CreatedByEventID int64                         `json:"created_by_event_id"`
	DecisionEventID  int64                         `json:"decision_event_id,omitempty"`
	ProposedBy       string                        `json:"proposed_by"`
	DecidedBy        string                        `json:"decided_by,omitempty"`
	Workspace        string                        `json:"workspace"`
	CreatedAt        time.Time                     `json:"created_at"`
	UpdatedAt        time.Time                     `json:"updated_at"`
}

type TaskContextProposalInput struct {
	TaskID        string                        `json:"task_id"`
	TargetKind    TaskContextProposalTargetKind `json:"target_kind"`
	TargetID      string                        `json:"target_id"`
	Source        TaskContextProposalSource     `json:"source"`
	Justification string                        `json:"justification"`
}

func (i TaskContextProposalInput) Valid() bool {
	return strings.TrimSpace(i.TaskID) != "" && i.TargetKind.Valid() && strings.TrimSpace(i.TargetID) != "" &&
		i.Source.Valid() && strings.TrimSpace(i.Justification) != ""
}
