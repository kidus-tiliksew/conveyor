package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
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

// NormalizeRequirementVersionDocument enforces that the denormalized statement
// JSON is exactly the machine fence stored in Content. Older callers that pass
// prose plus statements are normalized into the canonical fenced document at
// this persistence boundary. Historical feature-migration seeds are the sole
// exception: migration 046 intentionally preserved their prose verbatim.
func NormalizeRequirementVersionDocument(version *core.RequirementVersion) error {
	if version.Origin == core.RequirementOriginFeatureMigration && len(version.Statements) == 0 {
		return nil
	}
	document, err := pipeline.ParseRequirementDocument(version.Content)
	if err != nil {
		if !strings.Contains(version.Content, "```conveyor:") {
			document, err = pipeline.RenderRequirementDocument(version.Content, version.Statements)
			if err == nil {
				version.Content = document.Markdown
				return nil
			}
		}
		return fmt.Errorf("requirement content/statement coherence: %w", err)
	}
	if !reflect.DeepEqual(document.Statements, version.Statements) {
		return fmt.Errorf("requirement content/statement coherence: stored statements diverge from the conveyor:requirements fence")
	}
	return nil
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
			return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("%w: %s", ErrRequirementSlugConflict, requirement.Slug)
		}
	}
	if err := core.ValidateRequirementOrigin(first); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := core.ValidateRequirementStatements(first.Statements); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := NormalizeRequirementVersionDocument(&first); err != nil {
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
	if err := NormalizeRequirementVersionDocument(&version); err != nil {
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

func (m *memory) ConfirmRequirementVersion(ctx context.Context, requirementID string, version int, expectedCurrentVersion ...int) (core.Requirement, core.RequirementVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: requirementID}
	requirement, ok := m.requirements[key]
	if !ok {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s not found", requirementID)
	}
	versions := m.requirementVersions[key]
	if len(expectedCurrentVersion) > 1 {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("at most one expected current requirement version may be supplied")
	}
	if len(expectedCurrentVersion) == 1 && expectedCurrentVersion[0] != requirement.CurrentVersion {
		expected := expectedCurrentVersion[0]
		return core.Requirement{}, core.RequirementVersion{}, &RequirementVersionConflict{
			RequirementID: requirementID, Requested: version, Current: requirement.CurrentVersion, Expected: &expected,
		}
	}
	if version < 1 || version > len(versions) {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement %s has no version %d", requirementID, version)
	}
	index := version - 1
	confirmed := versions[index]
	if confirmed.Confirmed && version == requirement.CurrentVersion {
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
		return core.Requirement{}, core.RequirementVersion{}, &RequirementVersionConflict{
			RequirementID: requirementID, Requested: version, Current: requirement.CurrentVersion,
		}
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

func requirementServesKey(workspace, blueprintTaskID, requirementID string) string {
	return workspace + "\x00" + blueprintTaskID + "\x00" + requirementID
}

func (m *memory) ProposeRequirementServes(ctx context.Context, blueprintTaskID, requirementID string, source core.RequirementServesSource, confirm bool) (core.RequirementServesLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	blueprintTaskID, requirementID = strings.TrimSpace(blueprintTaskID), strings.TrimSpace(requirementID)
	if !source.Valid() {
		return core.RequirementServesLink{}, fmt.Errorf("invalid requirement serves source %q", source)
	}
	task, exists := m.tasks[blueprintTaskID]
	if !exists || task.Workspace != workspace {
		return core.RequirementServesLink{}, fmt.Errorf("blueprint task %s not found", blueprintTaskID)
	}
	requirement, exists := m.requirements[memoryScopedKey{workspace: workspace, id: requirementID}]
	if !exists {
		return core.RequirementServesLink{}, fmt.Errorf("requirement %s not found", requirementID)
	}
	key := requirementServesKey(workspace, blueprintTaskID, requirementID)
	if existing, ok := m.requirementServes[key]; ok {
		if confirm && existing.State == core.RequirementServesProposed {
			return m.confirmRequirementServesLocked(ctx, key, existing)
		}
		if existing.State == core.RequirementServesDismissed {
			return core.RequirementServesLink{}, fmt.Errorf("%w: cannot repropose a dismissed link", ErrRequirementServesTransition)
		}
		return existing, nil
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	eventKind := "requirement.serves_proposed"
	if source == core.RequirementServesPlanning || source == core.RequirementServesTriage {
		eventKind = "task.requirement_suggested"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: blueprintTaskID, Kind: eventKind, At: now, Payload: core.JSONPayload(map[string]any{
		"requirement_id": requirement.ID, "requirement_slug": requirement.Slug,
		"requirement_title": requirement.Title, "source": source,
	})})
	link := core.RequirementServesLink{
		BlueprintTaskID: blueprintTaskID, RequirementID: requirementID,
		State: core.RequirementServesProposed, Source: source,
		CreatedByEventID: m.nextEventID, ProposedBy: actor.ID,
		Workspace: workspace, CreatedAt: now, UpdatedAt: now,
	}
	m.requirementServes[key] = link
	if confirm {
		return m.confirmRequirementServesLocked(ctx, key, link)
	}
	return link, nil
}

func (m *memory) ConfirmRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := requirementServesKey(workspaceOrDefault(ctx, ""), strings.TrimSpace(blueprintTaskID), strings.TrimSpace(requirementID))
	link, ok := m.requirementServes[key]
	if !ok {
		return core.RequirementServesLink{}, fmt.Errorf("requirement serves proposal %s -> %s not found", blueprintTaskID, requirementID)
	}
	return m.confirmRequirementServesLocked(ctx, key, link)
}

func (m *memory) confirmRequirementServesLocked(ctx context.Context, key string, link core.RequirementServesLink) (core.RequirementServesLink, error) {
	if link.State == core.RequirementServesConfirmed {
		return link, nil
	}
	if link.State != core.RequirementServesProposed {
		return core.RequirementServesLink{}, fmt.Errorf("%w: cannot confirm %s link", ErrRequirementServesTransition, link.State)
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	m.appendEventLocked(ctx, core.Event{TaskID: link.BlueprintTaskID, Kind: "requirement.serves_confirmed", At: now, Payload: core.JSONPayload(map[string]any{
		"requirement_id": link.RequirementID, "confirmed_by": actor.ID,
	})})
	link.State, link.DecisionEventID, link.DecidedBy, link.UpdatedAt = core.RequirementServesConfirmed, m.nextEventID, actor.ID, now
	m.requirementServes[key] = link
	return link, nil
}

func (m *memory) DismissRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := requirementServesKey(workspaceOrDefault(ctx, ""), strings.TrimSpace(blueprintTaskID), strings.TrimSpace(requirementID))
	link, ok := m.requirementServes[key]
	if !ok {
		return core.RequirementServesLink{}, fmt.Errorf("requirement serves proposal %s -> %s not found", blueprintTaskID, requirementID)
	}
	if link.State == core.RequirementServesDismissed {
		return link, nil
	}
	if link.State != core.RequirementServesProposed {
		return core.RequirementServesLink{}, fmt.Errorf("%w: cannot dismiss %s link", ErrRequirementServesTransition, link.State)
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	m.appendEventLocked(ctx, core.Event{TaskID: link.BlueprintTaskID, Kind: "requirement.serves_dismissed", At: now, Payload: core.JSONPayload(map[string]any{
		"requirement_id": link.RequirementID, "dismissed_by": actor.ID,
	})})
	link.State, link.DecisionEventID, link.DecidedBy, link.UpdatedAt = core.RequirementServesDismissed, m.nextEventID, actor.ID, now
	m.requirementServes[key] = link
	return link, nil
}

func (m *memory) ListRequirementServes(ctx context.Context) ([]core.RequirementServesLink, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	links := []core.RequirementServesLink{}
	for _, link := range m.requirementServes {
		if link.Workspace == workspace {
			links = append(links, link)
		}
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].BlueprintTaskID != links[j].BlueprintTaskID {
			return links[i].BlueprintTaskID < links[j].BlueprintTaskID
		}
		return links[i].RequirementID < links[j].RequirementID
	})
	return links, nil
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
	session.PinnedRevisions = cloneStringMap(session.PinnedRevisions)
	m.planningSessions[key] = clonePlanningSession(session)
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.created", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": session.ID, "title": session.Title,
		"requirement_context_id": session.RequirementContextID,
	})})
	return clonePlanningSession(session), nil
}

func (m *memory) GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.planningSessions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", id)
	}
	return clonePlanningSession(session), nil
}

func (m *memory) ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.PlanningSession{}
	for key, session := range m.planningSessions {
		if key.workspace == workspace {
			out = append(out, clonePlanningSession(session))
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

func (m *memory) PinPlanningSessionRepo(ctx context.Context, sessionID, repo, revision string) (core.PlanningSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: sessionID}
	session, ok := m.planningSessions[key]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", sessionID)
	}
	if session.Status != core.PlanningSessionActive {
		return core.PlanningSession{}, fmt.Errorf("planning session %s is %s and cannot pin repositories", sessionID, session.Status)
	}
	repo, revision = strings.TrimSpace(repo), strings.TrimSpace(revision)
	if repo == "" || revision == "" {
		return core.PlanningSession{}, fmt.Errorf("planning repository and revision are required")
	}
	session.PinnedRevisions = cloneStringMap(session.PinnedRevisions)
	if existing := session.PinnedRevisions[repo]; existing != "" {
		if existing != revision {
			return core.PlanningSession{}, fmt.Errorf(
				"planning repository %s is already pinned at %s; cannot repin at %s", repo, existing, revision)
		}
		return clonePlanningSession(session), nil
	}
	session.PinnedRevisions[repo] = revision
	session.UpdatedAt = time.Now().UTC()
	m.planningSessions[key] = clonePlanningSession(session)
	m.appendEventLocked(ctx, core.Event{Kind: "planning_session.repo_pinned", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "session_id": sessionID, "repo": repo, "revision": revision,
	})})
	return clonePlanningSession(session), nil
}

func (m *memory) RecordPlanningExplorationTokens(ctx context.Context, sessionID string, tokens int) (core.PlanningSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: sessionID}
	session, ok := m.planningSessions[key]
	if !ok {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", sessionID)
	}
	if tokens < 0 {
		return core.PlanningSession{}, fmt.Errorf("planning exploration tokens must not be negative")
	}
	session.ExplorationTokensUsed += tokens
	session.UpdatedAt = time.Now().UTC()
	m.planningSessions[key] = clonePlanningSession(session)
	return clonePlanningSession(session), nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func clonePlanningSession(session core.PlanningSession) core.PlanningSession {
	session.PinnedRevisions = cloneStringMap(session.PinnedRevisions)
	return session
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
			return clonePlanningSession(session), nil
		}
		return core.PlanningSession{}, fmt.Errorf(
			"planning session %s is already finalized with different lineage", request.SessionID)
	}
	if session.Status != core.PlanningSessionActive {
		// Abandonment is terminal. In particular, an in-flight planning run
		// must not resurrect a session after the abandon request wins this
		// lock (spec §9).
		return core.PlanningSession{}, fmt.Errorf(
			"planning session %s is %s and cannot be finalized", request.SessionID, session.Status)
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
	if session.RequirementContextID != "" && session.ProducedTaskID != "" {
		servesKey := requirementServesKey(workspace, session.ProducedTaskID, session.RequirementContextID)
		if _, exists := m.requirementServes[servesKey]; !exists {
			requirement := m.requirements[memoryScopedKey{workspace: workspace, id: session.RequirementContextID}]
			m.appendEventLocked(ctx, core.Event{TaskID: session.ProducedTaskID, Kind: "task.requirement_suggested", At: now, Payload: core.JSONPayload(map[string]any{
				"requirement_id": requirement.ID, "requirement_slug": requirement.Slug,
				"requirement_title": requirement.Title, "source": core.RequirementServesPlanning,
			})})
			m.requirementServes[servesKey] = core.RequirementServesLink{
				BlueprintTaskID: session.ProducedTaskID, RequirementID: requirement.ID,
				State: core.RequirementServesProposed, Source: core.RequirementServesPlanning,
				CreatedByEventID: m.nextEventID, ProposedBy: ActorFromContext(ctx).ID,
				Workspace: workspace, CreatedAt: now, UpdatedAt: now,
			}
		}
	}
	return clonePlanningSession(session), nil
}

func (m *memory) AbandonPlanningSession(ctx context.Context, sessionID string) (core.PlanningSession, error) {
	var abandoned core.PlanningSession
	err := m.withPlanningSessionLock(ctx, sessionID, func(lockedCtx context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		workspace := workspaceOrDefault(lockedCtx, "")
		key := memoryScopedKey{workspace: workspace, id: sessionID}
		session, ok := m.planningSessions[key]
		if !ok {
			return fmt.Errorf("planning session %s not found", sessionID)
		}
		if session.Status == core.PlanningSessionAbandoned {
			abandoned = clonePlanningSession(session)
			return nil
		}
		if session.Status == core.PlanningSessionFinalized {
			// Abandoning would strand what the session produced.
			return fmt.Errorf("planning session %s is already finalized", sessionID)
		}
		session.Status = core.PlanningSessionAbandoned
		session.UpdatedAt = time.Now().UTC()
		m.planningSessions[key] = session
		m.appendEventLocked(lockedCtx, core.Event{Kind: "planning_session.abandoned", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace, "session_id": session.ID,
		})})
		abandoned = clonePlanningSession(session)
		return nil
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return clonePlanningSession(abandoned), nil
}
