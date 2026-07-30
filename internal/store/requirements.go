package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Requirement and planning-session persistence for the in-memory store.
// Requirements are versioned and confirmed, never gated (spec §4.2 item 1);
// planning sessions are durable and produce at most one artifact (spec §9).

// PlanningFinalizeRequest finalizes a session against exactly one produced
// artifact. A finalized session never carries approval authority: producing a
// blueprint parent task leaves the §13.1 spec gate untouched.
type PlanningFinalizeRequest struct {
	SessionID string
	// RequirementID and TaskID are mutually exclusive.
	RequirementID        string
	TaskID               string
	TranscriptArtifactID string
}

// Validate keeps the produced-artifact contract exclusive. A session that
// produced nothing is abandoned, not finalized.
func (r PlanningFinalizeRequest) Validate() error {
	if r.RequirementID != "" && r.TaskID != "" {
		return fmt.Errorf("planning session produces a requirement or a blueprint task, never both")
	}
	if r.RequirementID == "" && r.TaskID == "" {
		return fmt.Errorf("planning session finalize requires a produced requirement or blueprint task")
	}
	return nil
}

func (m *memory) CreateRequirement(ctx context.Context, requirement core.Requirement, first core.RequirementVersion) (core.Requirement, core.RequirementVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, requirement.Workspace)
	if requirement.ID == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement id is required")
	}
	if requirement.Title == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement title is required")
	}
	if requirement.Slug == "" {
		requirement.Slug = core.RequirementSlug(requirement.Title)
	}
	key := memoryScopedKey{workspace: workspace, id: requirement.ID}
	if _, exists := m.requirements[key]; exists {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s already exists", requirement.ID)
	}
	for existingKey, existing := range m.requirements {
		if existingKey.workspace == workspace && existing.Slug == requirement.Slug {
			return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement slug %s already exists", requirement.Slug)
		}
	}
	if err := core.ValidateRequirementOrigin(first); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := core.ValidateRequirementStatements(first.Statements); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	now := time.Now().UTC()
	requirement.Workspace = workspace
	// current_version stays zero: a new document is visibly pending, never
	// silently authoritative.
	requirement.CurrentVersion = 0
	requirement.StatementHighWaterMark = core.RequirementStatementHighWaterMark(first.Statements)
	if requirement.CreatedAt.IsZero() {
		requirement.CreatedAt = now
	}
	requirement.UpdatedAt = now

	first.Workspace = workspace
	first.RequirementID = requirement.ID
	first.Version = 1
	first.Confirmed = false
	first.ConfirmedBy = ""
	first.ConfirmedAt = time.Time{}
	if first.CreatedAt.IsZero() {
		first.CreatedAt = now
	}

	m.requirements[key] = requirement
	m.requirementVersions[key] = []core.RequirementVersion{first}
	m.appendEventLocked(ctx, core.Event{Kind: "requirement.created", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "requirement_id": requirement.ID,
		"slug": requirement.Slug, "title": requirement.Title,
	})})
	m.appendEventLocked(ctx, core.Event{Kind: "requirement.version_proposed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "requirement_id": requirement.ID, "version": first.Version,
		"origin": first.Origin, "origin_session_id": first.OriginSessionID, "origin_drift_id": first.OriginDriftID,
		"statement_count": len(first.Statements),
	})})
	return requirement, first, nil
}

func (m *memory) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	requirement, ok := m.requirements[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.Requirement{}, fmt.Errorf("requirement %s not found", id)
	}
	return requirement, nil
}

func (m *memory) ListRequirements(ctx context.Context) ([]core.Requirement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.Requirement{}
	for key, requirement := range m.requirements {
		if key.workspace == workspace {
			out = append(out, requirement)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memory) ProposeRequirementVersion(ctx context.Context, version core.RequirementVersion) (core.RequirementVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, version.Workspace)
	key := memoryScopedKey{workspace: workspace, id: version.RequirementID}
	requirement, ok := m.requirements[key]
	if !ok {
		return core.RequirementVersion{}, fmt.Errorf("requirement %s not found", version.RequirementID)
	}
	if err := core.ValidateRequirementOrigin(version); err != nil {
		return core.RequirementVersion{}, err
	}
	existing := m.requirementVersions[key]
	// Every REQ-n the document has ever issued, so reinstating a statement that
	// an unconfirmed proposal dropped is not mistaken for identifier reuse.
	var issued []string
	for _, previous := range existing {
		for _, statement := range previous.Statements {
			issued = append(issued, statement.ID)
		}
	}
	if err := core.ValidateRequirementRevision(requirement.StatementHighWaterMark, issued, version.Statements); err != nil {
		return core.RequirementVersion{}, err
	}
	now := time.Now().UTC()
	version.Workspace = workspace
	version.Version = len(existing) + 1
	// A proposal is never born confirmed, whatever the caller supplied.
	version.Confirmed = false
	version.ConfirmedBy = ""
	version.ConfirmedAt = time.Time{}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = now
	}
	m.requirementVersions[key] = append(existing, version)
	if mark := core.RequirementStatementHighWaterMark(version.Statements); mark > requirement.StatementHighWaterMark {
		requirement.StatementHighWaterMark = mark
	}
	requirement.UpdatedAt = now
	m.requirements[key] = requirement
	m.appendEventLocked(ctx, core.Event{Kind: "requirement.version_proposed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "requirement_id": version.RequirementID, "version": version.Version,
		"origin": version.Origin, "origin_session_id": version.OriginSessionID, "origin_drift_id": version.OriginDriftID,
		"statement_count": len(version.Statements),
	})})
	return version, nil
}

func (m *memory) ConfirmRequirementVersion(ctx context.Context, requirementID string, version int) (core.Requirement, core.RequirementVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: requirementID}
	requirement, ok := m.requirements[key]
	if !ok {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s not found", requirementID)
	}
	versions := m.requirementVersions[key]
	if version < 1 || version > len(versions) {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s has no version %d", requirementID, version)
	}
	index := version - 1
	confirmed := versions[index]
	if confirmed.Confirmed {
		return requirement, confirmed, nil
	}
	// Confirmation is where a real statement block becomes mandatory, so a
	// migration seed cannot become current intent unedited.
	if err := core.ConfirmableRequirementVersion(confirmed); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	// Confirmation moves forward only. Re-confirming a superseded version would
	// silently revert intent the operator already advanced past.
	if version < requirement.CurrentVersion {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s already confirmed version %d; cannot confirm earlier version %d", requirementID, requirement.CurrentVersion, version)
	}
	actor := ActorFromContext(ctx)
	now := time.Now().UTC()
	confirmed.Confirmed = true
	confirmed.ConfirmedBy = actor.ID
	confirmed.ConfirmedAt = now
	versions[index] = confirmed
	m.requirementVersions[key] = versions
	requirement.CurrentVersion = version
	requirement.UpdatedAt = now
	m.requirements[key] = requirement
	m.appendEventLocked(ctx, core.Event{Kind: "requirement.version_confirmed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "requirement_id": requirementID, "version": version,
		"origin": confirmed.Origin, "confirmed_by": actor.ID,
	})})
	return requirement, confirmed, nil
}

func (m *memory) GetRequirementVersion(ctx context.Context, requirementID string, version int) (core.RequirementVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.requirementVersions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: requirementID}]
	if version < 1 || version > len(versions) {
		return core.RequirementVersion{}, fmt.Errorf("requirement %s has no version %d", requirementID, version)
	}
	return versions[version-1], nil
}

func (m *memory) ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored := m.requirementVersions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: requirementID}]
	out := make([]core.RequirementVersion, len(stored))
	copy(out, stored)
	return out, nil
}

func (m *memory) CreatePlanningSession(ctx context.Context, session core.PlanningSession) (core.PlanningSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, session.Workspace)
	if session.ID == "" {
		return core.PlanningSession{}, fmt.Errorf("planning session id is required")
	}
	key := memoryScopedKey{workspace: workspace, id: session.ID}
	if _, exists := m.planningSessions[key]; exists {
		return core.PlanningSession{}, fmt.Errorf("planning session %s already exists", session.ID)
	}
	if session.RequirementContextID != "" {
		if _, ok := m.requirements[memoryScopedKey{workspace: workspace, id: session.RequirementContextID}]; !ok {
			return core.PlanningSession{}, fmt.Errorf("requirement %s not found", session.RequirementContextID)
		}
	}
	now := time.Now().UTC()
	session.Workspace = workspace
	session.Status = core.PlanningSessionActive
	session.ProducedRequirementID = ""
	session.ProducedTaskID = ""
	session.TranscriptArtifactID = ""
	session.FinalizedAt = time.Time{}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	m.planningSessions[key] = session
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.created", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": session.ID, "title": session.Title,
		"requirement_context_id": session.RequirementContextID,
	})})
	return session, nil
}

func (m *memory) GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.planningSessions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", id)
	}
	return session, nil
}

func (m *memory) ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.PlanningSession{}
	for key, session := range m.planningSessions {
		if key.workspace == workspace {
			out = append(out, session)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memory) AppendPlanningMessage(ctx context.Context, message core.PlanningMessage) (core.PlanningMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, message.Workspace)
	key := memoryScopedKey{workspace: workspace, id: message.SessionID}
	session, ok := m.planningSessions[key]
	if !ok {
		return core.PlanningMessage{}, fmt.Errorf("planning session %s not found", message.SessionID)
	}
	if session.Status != core.PlanningSessionActive {
		return core.PlanningMessage{}, fmt.Errorf("planning session %s is %s and accepts no further messages", message.SessionID, session.Status)
	}
	if !message.Role.Valid() {
		return core.PlanningMessage{}, fmt.Errorf("invalid planning message role %q", message.Role)
	}
	now := time.Now().UTC()
	existing := m.planningMessages[key]
	message.Workspace = workspace
	message.Seq = len(existing) + 1
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	m.planningMessages[key] = append(existing, message)
	session.UpdatedAt = now
	m.planningSessions[key] = session
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.message_appended", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": message.SessionID,
		"seq": message.Seq, "role": message.Role,
	})})
	return message, nil
}

func (m *memory) ListPlanningMessages(ctx context.Context, sessionID string) ([]core.PlanningMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored := m.planningMessages[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: sessionID}]
	out := make([]core.PlanningMessage, len(stored))
	copy(out, stored)
	return out, nil
}

func (m *memory) FinalizePlanningSession(ctx context.Context, request PlanningFinalizeRequest) (core.PlanningSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: request.SessionID}
	session, ok := m.planningSessions[key]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", request.SessionID)
	}
	if err := request.Validate(); err != nil {
		return core.PlanningSession{}, err
	}
	if session.Status == core.PlanningSessionFinalized {
		// Idempotent for an identical finalize; any difference in the recorded
		// lineage — produced artifact or archived transcript — is a
		// contradiction, not a retry, so it must not overwrite what was stored.
		if session.ProducedRequirementID == request.RequirementID &&
			session.ProducedTaskID == request.TaskID &&
			session.TranscriptArtifactID == request.TranscriptArtifactID {
			return session, nil
		}
		return core.PlanningSession{}, fmt.Errorf(
			"planning session %s is already finalized with different lineage", request.SessionID)
	}
	if request.RequirementID != "" {
		if _, exists := m.requirements[memoryScopedKey{workspace: workspace, id: request.RequirementID}]; !exists {
			return core.PlanningSession{}, fmt.Errorf("requirement %s not found", request.RequirementID)
		}
	}
	if request.TaskID != "" {
		// m.tasks is keyed by id alone, so the workspace has to be compared
		// explicitly. Postgres enforces the same thing structurally through the
		// composite (workspace_id, produced_task_id) foreign key.
		task, exists := m.tasks[request.TaskID]
		if !exists || task.Workspace != workspace {
			return core.PlanningSession{}, fmt.Errorf("task %s not found", request.TaskID)
		}
	}
	if request.TranscriptArtifactID != "" {
		if _, exists := m.artifacts[memoryArtifactKey{workspace: workspace, id: request.TranscriptArtifactID}]; !exists {
			return core.PlanningSession{}, fmt.Errorf("artifact %s not found", request.TranscriptArtifactID)
		}
	}
	now := time.Now().UTC()
	session.Status = core.PlanningSessionFinalized
	session.ProducedRequirementID = request.RequirementID
	session.ProducedTaskID = request.TaskID
	session.TranscriptArtifactID = request.TranscriptArtifactID
	session.FinalizedAt = now
	session.UpdatedAt = now
	m.planningSessions[key] = session
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.finalized", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": session.ID,
		"produced_requirement_id": session.ProducedRequirementID,
		"produced_task_id":        session.ProducedTaskID,
		"transcript_artifact_id":  session.TranscriptArtifactID,
	})})
	return session, nil
}

func (m *memory) AbandonPlanningSession(ctx context.Context, sessionID string) (core.PlanningSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: sessionID}
	session, ok := m.planningSessions[key]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", sessionID)
	}
	if session.Status == core.PlanningSessionAbandoned {
		return session, nil
	}
	if session.Status == core.PlanningSessionFinalized {
		// Abandoning would strand what the session produced.
		return core.PlanningSession{}, fmt.Errorf("planning session %s is already finalized", sessionID)
	}
	session.Status = core.PlanningSessionAbandoned
	session.UpdatedAt = time.Now().UTC()
	m.planningSessions[key] = session
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.abandoned", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": session.ID,
	})})
	return session, nil
}
