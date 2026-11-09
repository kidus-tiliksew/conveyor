package store

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskTriggerProvenance names the agent credential that filed a task and the
// user who owns it. Only task.created events recorded before the class was
// retired carry it, and nothing writes it now (DEC-116(1), (3)).
type TaskTriggerProvenance struct {
	AgentCredentialID string `json:"agent_credential_id"`
	OwnerUserID       string `json:"owner_user_id"`
}

// TaskCreatedEvent retains only creation-time values consumed by event folds.
// Mutable task state, policy, assignee and intake-context IDs must remain
// historical; document bodies and live display fields belong to their tables.
// Field names also decode existing full-task creation events.
type TaskCreatedEvent struct {
	ID            string         `json:"id"`
	State         core.TaskState `json:"state"`
	NextStage     core.Stage     `json:"next_stage,omitempty"`
	SpecApproval  bool           `json:"spec_approval"`
	MergeApproval bool           `json:"merge_approval"`
	// PlanState is the plan state the task was created with, and PlanVersion
	// the plan version an intake plan became; an older payload carries
	// neither (component-task-lifecycle invariant 22).
	PlanState      core.PlanState `json:"plan_state,omitempty"`
	PlanVersion    int            `json:"plan_version,omitempty"`
	PolicyContract struct {
		VerifyStage bool `json:"verify_stage"`
	} `json:"policy_contract"`
	Assignee          *core.TaskAssignee `json:"assignee,omitempty"`
	ParentTaskID      string             `json:"parent_task_id,omitempty"`
	OriginSpecVersion int                `json:"origin_spec_version,omitempty"`
	Context           struct {
		Requirements []taskCreatedReference `json:"requirements,omitempty"`
		Designs      []taskCreatedReference `json:"designs,omitempty"`
	} `json:"context"`
	TriggerProvenance *TaskTriggerProvenance `json:"trigger_provenance,omitempty"`
	// Redactions counts what redaction removed from this task's title by
	// class and never by value (req-security-boundaries AC-3.1).
	Redactions *core.RedactionStats `json:"redactions,omitempty"`
}

type taskCreatedReference struct {
	ID string `json:"id"`
}

// TaskCreatedPayload is the reference-only task.created payload every creation
// transaction writes, including blueprint children and planning-bundle tasks.
func TaskCreatedPayload(ctx context.Context, task core.Task) json.RawMessage {
	payload := TaskCreatedEvent{
		ID: task.ID, State: task.State, NextStage: task.NextStage,
		SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval, PlanState: task.PlanState,
		ParentTaskID: task.ParentTaskID, OriginSpecVersion: task.OriginSpecVersion,
	}
	payload.PolicyContract.VerifyStage = task.SetupContract.VerifyStage
	if task.IntakePlan != nil {
		payload.PlanVersion = core.IntakePlanVersion
	}
	if task.Assignee != nil {
		payload.Assignee = &core.TaskAssignee{UserID: task.Assignee.UserID}
	}
	if len(task.Context.Requirements) > 0 {
		payload.Context.Requirements = make([]taskCreatedReference, len(task.Context.Requirements))
		for i, reference := range task.Context.Requirements {
			payload.Context.Requirements[i].ID = reference.ID
		}
	}
	if len(task.Context.Designs) > 0 {
		payload.Context.Designs = make([]taskCreatedReference, len(task.Context.Designs))
		for i, reference := range task.Context.Designs {
			payload.Context.Designs[i].ID = reference.ID
		}
	}
	if stats, ok := ctx.Value(taskTitleRedactionsKey{}).(core.RedactionStats); ok {
		payload.Redactions = &stats
	}
	return core.JSONPayload(payload)
}

// TaskCreatedProvenance decodes trigger provenance from a task.created
// payload. Only a payload an agent credential's creation recorded carries it;
// every other payload, and every payload written since DEC-116, reports none.
func TaskCreatedProvenance(payload json.RawMessage) (TaskTriggerProvenance, bool, error) {
	var decoded struct {
		TriggerProvenance *TaskTriggerProvenance `json:"trigger_provenance"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return TaskTriggerProvenance{}, false, err
	}
	if decoded.TriggerProvenance == nil {
		return TaskTriggerProvenance{}, false, nil
	}
	return *decoded.TriggerProvenance, true, nil
}
