package store

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskTriggerProvenance names the agent credential that filed a task and the
// user who owns it (req-intake-and-triage AC-4.2; DEC-60).
type TaskTriggerProvenance struct {
	AgentCredentialID string `json:"agent_credential_id"`
	OwnerUserID       string `json:"owner_user_id"`
}

// TaskCreatedEvent retains only creation-time values consumed by event folds.
// Mutable task state, policy, assignee and intake-context IDs must remain
// historical; document bodies and live display fields belong to their tables.
// Field names also decode existing full-task creation events.
type TaskCreatedEvent struct {
	ID             string         `json:"id"`
	State          core.TaskState `json:"state"`
	NextStage      core.Stage     `json:"next_stage,omitempty"`
	SpecApproval   bool           `json:"spec_approval"`
	MergeApproval  bool           `json:"merge_approval"`
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
}

type taskCreatedReference struct {
	ID string `json:"id"`
}

// TaskCreationProvenance derives trigger provenance from the authenticated
// credential bound to ctx. Only a verified agent credential with both its own
// ID and its owning user yields provenance; request fields, the intake source,
// headers, and client-asserted actors never reach this function, and an
// incomplete identity never invents an owner (component-persistence).
func TaskCreationProvenance(ctx context.Context) (TaskTriggerProvenance, bool) {
	credential, ok := CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialAgent {
		return TaskTriggerProvenance{}, false
	}
	return TaskTriggerProvenance{AgentCredentialID: credential.ID, OwnerUserID: credential.OwnerUserID}, true
}

// TaskCreatedPayload is the reference-only task.created payload every creation
// transaction writes, including blueprint children and planning-bundle tasks.
func TaskCreatedPayload(ctx context.Context, task core.Task) json.RawMessage {
	payload := TaskCreatedEvent{
		ID: task.ID, State: task.State, NextStage: task.NextStage,
		SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval,
		ParentTaskID: task.ParentTaskID, OriginSpecVersion: task.OriginSpecVersion,
	}
	payload.PolicyContract.VerifyStage = task.SetupContract.VerifyStage
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
	if provenance, ok := TaskCreationProvenance(ctx); ok {
		payload.TriggerProvenance = &provenance
	}
	return core.JSONPayload(payload)
}

// TaskCreatedProvenance decodes trigger provenance from a task.created
// payload. Payloads written before DEC-60, and human or system creations,
// report none.
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
