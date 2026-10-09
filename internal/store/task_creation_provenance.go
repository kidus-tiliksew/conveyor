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

// taskCreatedPayload keeps every task field at the top level, so a reader
// that decodes task.created as core.Task still sees the same document, and
// appends the provenance beside it.
type taskCreatedPayload struct {
	core.Task
	TriggerProvenance *TaskTriggerProvenance `json:"trigger_provenance,omitempty"`
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

// TaskCreatedPayload is the task.created payload every backend's creation
// transaction writes. Human and system creations keep the plain task JSON;
// an agent creation adds trigger_provenance (component-persistence;
// component-http-api).
func TaskCreatedPayload(ctx context.Context, task core.Task) json.RawMessage {
	provenance, ok := TaskCreationProvenance(ctx)
	if !ok {
		return core.JSONPayload(task)
	}
	return core.JSONPayload(taskCreatedPayload{Task: task, TriggerProvenance: &provenance})
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
