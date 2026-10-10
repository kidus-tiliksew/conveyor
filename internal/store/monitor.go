package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

// ErrMonitorWorkspaceMismatch refuses a monitor record whose WorkspaceID is
// omitted or names a workspace other than the bound one.
var ErrMonitorWorkspaceMismatch = errors.New("monitor record workspace must match the bound workspace")

// MonitorRecordWorkspace is the one workspace rule for monitor record writes
// (Observe and RecordDrift) on every backend. The context must be bound, and
// the record must name exactly that workspace. Every production caller
// normalizes the record into the resolved workspace first, so an omitted
// record workspace is a mismatch rather than a default. Backends call it
// before any insert, deduplication, lookup, or event (component-monitor-drift;
// component-persistence).
func MonitorRecordWorkspace(ctx context.Context, record string) (string, error) {
	bound, ok := WorkspaceFromContext(ctx)
	if !ok || bound == "" {
		return "", ErrWorkspaceRequired
	}
	if record != bound {
		return "", fmt.Errorf("%w: record names %q, request is bound to %q", ErrMonitorWorkspaceMismatch, record, bound)
	}
	return bound, nil
}

func (m *memory) AuditTask(ctx context.Context, taskID, kind string, payload map[string]any) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	return m.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, Payload: core.JSONPayload(payload)})
}

func (m *memory) AuditMonitor(ctx context.Context, kind string, payload map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	m.nextMonitorActivityID++
	m.monitorActivity[workspace] = append(m.monitorActivity[workspace], monitor.Activity{
		ID: m.nextMonitorActivityID, WorkspaceID: workspace, Kind: kind,
		Payload: payload, At: time.Now().UTC(),
	})
	return nil
}

func monitorKey(workspace, identity string) string { return workspace + "\x00" + identity }

func (m *memory) WithMonitorSignalClassLock(ctx context.Context, repository string, kind monitor.SignalKind, fn func(context.Context) error) error {
	return m.WithTaskSideEffectLock(ctx, "monitor-signal:"+repository+":"+string(kind), fn)
}

func (m *memory) FindOpenMonitorTask(ctx context.Context, repository string, kind monitor.SignalKind) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	var selected core.Task
	for _, task := range m.tasks {
		if task.Workspace != workspace || task.Repo != repository || task.Source != "monitor:"+string(kind) || core.TaskTerminal(task.State) {
			continue
		}
		if selected.ID == "" || task.CreatedAt.Before(selected.CreatedAt) || (task.CreatedAt.Equal(selected.CreatedAt) && task.ID < selected.ID) {
			selected = task
		}
	}
	return selected.ID, selected.ID != "", nil
}

func (m *memory) RequirementExists(ctx context.Context, id string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.requirements[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	return exists, nil
}

func (m *memory) ResolveCausalSystemDesignMerge(ctx context.Context, documentID, repository, commitSHA string, causalEventID int64, driftID string, matchingPaths []string, recordConsulted bool) (monitor.SystemDesignMergeJudgment, error) {
	if _, err := RequireActor(ctx); err != nil {
		return monitor.SystemDesignMergeJudgment{}, err
	}
	if causalEventID <= 0 || strings.TrimSpace(commitSHA) == "" {
		return monitor.SystemDesignMergeJudgment{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	result := monitor.SystemDesignMergeJudgment{}
	var causal core.Event
	var causalTask core.Task
	for taskID, events := range m.events {
		if taskID == "" {
			continue
		}
		task, ok := m.tasks[taskID]
		if !ok || task.Workspace != workspace {
			continue
		}
		for _, event := range events {
			if event.ID == causalEventID {
				causal = event
				causalTask = task
				break
			}
		}
	}
	if causal.TaskID == "" || causalTask.Repo != strings.TrimSpace(repository) || (causal.Kind != "merge.confirmed" && causal.Kind != "merge.reconciled") {
		return result, nil
	}
	var merge struct {
		HeadSHA string `json:"head_sha"`
	}
	if json.Unmarshal(causal.Payload, &merge) != nil || merge.HeadSHA != strings.TrimSpace(commitSHA) {
		return result, nil
	}
	result.CausalEventValid = true
	// Causal windows compare chronological (at, id) tuples, never numeric IDs
	// alone, matching the durable backends (component-persistence; DEC-39).
	var latest int64
	var latestEvent core.Event
	var proposalVersion int
	for _, event := range m.events[""] {
		if !TaskEventBefore(event, causal) || event.Kind != "system_design.version_proposed" {
			continue
		}
		var proposal struct {
			WorkspaceID  string `json:"workspace_id"`
			DocumentID   string `json:"document_id"`
			OriginTaskID string `json:"origin_task_id"`
			Version      int    `json:"version"`
		}
		if json.Unmarshal(event.Payload, &proposal) == nil && proposal.WorkspaceID == workspace && proposal.DocumentID == documentID && proposal.OriginTaskID == causal.TaskID && (latest == 0 || TaskEventBefore(latestEvent, event)) {
			latest, latestEvent = event.ID, event
			proposalVersion = proposal.Version
		}
	}
	if latest != 0 {
		consumed := false
		for _, activity := range m.monitorActivity[workspace] {
			if activity.Kind != "system_design.drift_suppressed" || payloadInt64(activity.Payload["proposal_event_id"]) != latest {
				continue
			}
			consumed = true
			if fmt.Sprint(activity.Payload["drift_id"]) == driftID {
				result.Proposal = monitor.ProposalSuppression{EventID: latest, Status: fmt.Sprint(activity.Payload["proposal_status"])}
				return result, nil
			}
			break
		}
		if !consumed {
			status := "pending"
			versions := m.systemDesignVersions[memoryScopedKey{workspace: workspace, id: documentID}]
			if proposalVersion > 0 && proposalVersion <= len(versions) && versions[proposalVersion-1].Confirmed {
				status = "confirmed"
			}
			m.nextMonitorActivityID++
			m.monitorActivity[workspace] = append(m.monitorActivity[workspace], monitor.Activity{
				ID: m.nextMonitorActivityID, WorkspaceID: workspace, Kind: "system_design.drift_suppressed", At: time.Now().UTC(),
				Payload: map[string]any{"document_id": documentID, "drift_id": driftID, "merge_event_id": causalEventID, "proposal_event_id": latest, "proposal_status": status, "matching_paths": append([]string(nil), matchingPaths...)},
			})
			result.Proposal = monitor.ProposalSuppression{EventID: latest, Status: status}
			return result, nil
		}
	}

	causalEvents := make([]core.Event, 0, len(m.events[causal.TaskID]))
	for _, event := range m.events[causal.TaskID] {
		if TaskEventBefore(event, causal) {
			causalEvents = append(causalEvents, event)
		}
	}
	_, designs := ActiveTaskContextReferences(ChronologicalTaskEvents(causalEvents))
	result.AttachedVersion = designs[documentID]
	if !recordConsulted || result.AttachedVersion == 0 {
		return result, nil
	}
	for _, event := range m.events[""] {
		if event.Kind != "system_design.consulted" {
			continue
		}
		var payload struct {
			DocumentID   string `json:"document_id"`
			MergeEventID int64  `json:"merge_event_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.DocumentID == documentID && payload.MergeEventID == causalEventID {
			result.Consulted = true
			return result, nil
		}
	}
	m.appendEventLocked(ctx, core.Event{Kind: "system_design.consulted", At: causal.At, Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "document_id": documentID, "version": result.AttachedVersion,
		"delivery_task_id": causal.TaskID, "merge_event_id": causalEventID, "merge_head_sha": strings.TrimSpace(commitSHA),
		"matching_paths": append([]string(nil), matchingPaths...), "consultation": "delivery_no_revision",
	})})
	result.Consulted = true
	return result, nil
}

func payloadInt64(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func (m *memory) Observe(ctx context.Context, observation monitor.Observation) (monitor.ObservationRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, err := MonitorRecordWorkspace(ctx, observation.WorkspaceID)
	if err != nil {
		return monitor.ObservationRecord{}, false, err
	}
	key := monitorKey(workspace, observation.Identity())
	if current, ok := m.monitorObservations[key]; ok {
		if len(observation.ChangedPaths) > 0 {
			current.ChangedPaths = append([]string(nil), observation.ChangedPaths...)
		}
		if observation.CausalEventID > 0 {
			current.CausalEventID = observation.CausalEventID
		}
		current.DeduplicatedCount++
		current.UpdatedAt = observation.ObservedAt
		current.State = "deduplicated"
		m.monitorObservations[key] = current
		return current, false, nil
	}
	record := monitor.ObservationRecord{
		Observation: observation, State: "observed",
		CreatedAt: observation.ObservedAt, UpdatedAt: observation.ObservedAt,
	}
	m.monitorObservations[key] = record
	return record, true, nil
}

func (m *memory) LinkTask(ctx context.Context, identity, taskID, outcome string) (monitor.ObservationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.ObservationRecord{}, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, identity)
	record, ok := m.monitorObservations[key]
	if !ok {
		return monitor.ObservationRecord{}, fmt.Errorf("monitor observation %s not found", identity)
	}
	if record.TaskID != "" && record.TaskID != taskID {
		return monitor.ObservationRecord{}, fmt.Errorf("monitor observation %s already links task %s", identity, record.TaskID)
	}
	if record.TaskID == "" && record.Kind.Drift() && m.unresolvedTaskDriftCountLocked(workspace, taskID) >= monitor.MaxUnresolvedDriftPerTask {
		return monitor.ObservationRecord{}, monitor.TaskDriftSaturatedError(taskID)
	}
	record.TaskID, record.TaskOutcome, record.State, record.UpdatedAt = taskID, outcome, "task_linked", time.Now().UTC()
	m.monitorObservations[key] = record
	return record, nil
}

func (m *memory) RecordDrift(ctx context.Context, drift monitor.Drift) (monitor.Drift, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return monitor.Drift{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, err := MonitorRecordWorkspace(ctx, drift.WorkspaceID)
	if err != nil {
		return monitor.Drift{}, false, err
	}
	key := monitorKey(workspace, drift.ID)
	if current, ok := m.monitorDrift[key]; ok {
		return current, false, nil
	}
	if m.unresolvedTaskDriftCountLocked(workspace, drift.TaskID) >= monitor.MaxUnresolvedDriftPerTask {
		return monitor.Drift{}, false, monitor.TaskDriftSaturatedError(drift.TaskID)
	}
	m.monitorDrift[key] = drift
	if drift.SystemDesignID != "" {
		m.appendEventLocked(ctx, core.Event{Kind: "system_design.drift_detected", At: drift.DetectedAt, Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace, "document_id": drift.SystemDesignID, "version": drift.SystemDesignVersion,
			"drift_id": drift.ID, "causal_event_id": drift.CausalEventID, "matching_paths": drift.MatchingPaths,
		})})
	}
	return drift, true, nil
}

func (m *memory) unresolvedTaskDriftCountLocked(workspace, taskID string) int {
	count := 0
	prefix := workspace + "\x00"
	for key, drift := range m.monitorDrift {
		if strings.HasPrefix(key, prefix) && drift.TaskID == taskID && drift.ResolvedAt.IsZero() {
			count++
		}
	}
	return count
}

func (m *memory) reconcileConfirmedSystemDesignDriftLocked(ctx context.Context, documentID string, version int, proposedAt, resolvedAt time.Time) {
	workspace := workspaceOrDefault(ctx, "")
	prefix := workspace + "\x00"
	for key, drift := range m.monitorDrift {
		if !strings.HasPrefix(key, prefix) || drift.Kind != monitor.LineagedMerge || drift.SystemDesignID != documentID ||
			drift.SystemDesignVersion >= version || !drift.ResolvedAt.IsZero() || !drift.DetectedAt.Before(proposedAt) {
			continue
		}
		drift.Outcome, drift.ResolvedAt = "design_document_updated", resolvedAt
		m.monitorDrift[key] = drift
		m.appendEventLocked(ctx, core.Event{TaskID: drift.TaskID, Kind: "monitor.drift_reconciled", At: resolvedAt, Payload: core.JSONPayload(map[string]any{
			"drift_id": drift.ID, "outcome": drift.Outcome, "resolved_at": drift.ResolvedAt,
			"document_id": documentID, "confirmed_version": version,
		})})
		m.appendEventLocked(ctx, core.Event{Kind: "system_design.drift_resolved", At: resolvedAt, Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace, "document_id": documentID, "drift_id": drift.ID,
			"outcome": drift.Outcome, "resolved_at": drift.ResolvedAt, "confirmed_version": version,
		})})
	}
}

func (m *memory) ResolveDrift(ctx context.Context, id, outcome, requirementID string) (monitor.Drift, error) {
	if _, err := RequireActor(ctx); err != nil {
		return monitor.Drift{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.Drift{}, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, id)
	drift, ok := m.monitorDrift[key]
	if !ok {
		return monitor.Drift{}, fmt.Errorf("drift %s not found", id)
	}
	outcome = strings.TrimSpace(outcome)
	requirementID = strings.TrimSpace(requirementID)
	if outcome != "requirements_amended" && outcome != "design_document_updated" && outcome != "conflict_resolved" && outcome != "change_reverted" {
		return monitor.Drift{}, fmt.Errorf("unsupported audited reconciliation outcome %q", outcome)
	}
	if outcome != "requirements_amended" && requirementID != "" {
		return monitor.Drift{}, fmt.Errorf("%w: outcome %s", monitor.ErrRequirementIDNotAllowed, outcome)
	}
	if !drift.ResolvedAt.IsZero() {
		if requirementID != "" && requirementID != drift.RequirementID {
			return monitor.Drift{}, fmt.Errorf("%w: drift %s is linked to requirement %s", monitor.ErrRequirementIDInvalid, id, drift.RequirementID)
		}
		return drift, nil
	}
	now := time.Now().UTC()
	if outcome == "requirements_amended" {
		return m.proposeDriftAmendmentLocked(ctx, workspace, key, drift, requirementID, now)
	}
	if outcome == "design_document_updated" {
		if drift.SystemDesignID == "" {
			return monitor.Drift{}, fmt.Errorf("drift %s cannot resolve as design_document_updated without a system design", id)
		}
		designKey := memoryScopedKey{workspace: workspace, id: drift.SystemDesignID}
		document, exists := m.systemDesigns[designKey]
		if !exists || document.CurrentVersion <= drift.SystemDesignVersion {
			return monitor.Drift{}, fmt.Errorf("drift %s requires a confirmed replacement version for system design %s", id, drift.SystemDesignID)
		}
		versions := m.systemDesignVersions[designKey]
		if document.CurrentVersion > len(versions) || !versions[document.CurrentVersion-1].Confirmed {
			return monitor.Drift{}, fmt.Errorf("drift %s requires a confirmed replacement version for system design %s", id, drift.SystemDesignID)
		}
	}
	drift.Outcome, drift.ResolvedAt = outcome, now
	m.monitorDrift[key] = drift
	m.appendEventLocked(ctx, core.Event{TaskID: drift.TaskID, Kind: "monitor.drift_reconciled", At: now, Payload: core.JSONPayload(map[string]any{
		"drift_id": drift.ID, "outcome": drift.Outcome, "resolved_at": drift.ResolvedAt, "requirement_id": drift.RequirementID,
	})})
	if drift.SystemDesignID != "" {
		m.appendEventLocked(ctx, core.Event{Kind: "system_design.drift_resolved", At: now, Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace, "document_id": drift.SystemDesignID, "drift_id": drift.ID, "outcome": drift.Outcome, "resolved_at": drift.ResolvedAt,
		})})
	}
	return drift, nil
}

// proposeDriftAmendmentLocked records the requirements_amended outcome as a
// requirement proposal and leaves the drift open, linked to the proposed
// version through its origin_drift_id. Confirming that exact version closes the
// drift (reconcileConfirmedRequirementDriftLocked); dismissing it leaves the
// drift open for another proposal or outcome (DEC-46; req-delivery-and-forge
// AC-4.2, AC-4.3). A retry reuses the live pending amendment.
func (m *memory) proposeDriftAmendmentLocked(ctx context.Context, workspace, key string, drift monitor.Drift, requirementID string, now time.Time) (monitor.Drift, error) {
	if requirementID != "" && drift.RequirementID != "" && requirementID != drift.RequirementID {
		return monitor.Drift{}, fmt.Errorf("%w: drift %s is already linked to requirement %s", monitor.ErrRequirementIDInvalid, drift.ID, drift.RequirementID)
	}
	if drift.RequirementID == "" {
		drift.RequirementID = requirementID
	}
	if strings.TrimSpace(drift.RequirementID) == "" {
		return monitor.Drift{}, fmt.Errorf("%w: drift %s cannot resolve as requirements_amended", monitor.ErrRequirementIDMissing, drift.ID)
	}
	requirementKey := memoryScopedKey{workspace: workspace, id: drift.RequirementID}
	requirement, exists := m.requirements[requirementKey]
	if !exists {
		return monitor.Drift{}, fmt.Errorf("%w: %s", monitor.ErrUnknownRequirementID, drift.RequirementID)
	}
	if requirement.CurrentVersion == 0 {
		return monitor.Drift{}, fmt.Errorf("%w: requirement %s has no confirmed current version", monitor.ErrRequirementIDInvalid, drift.RequirementID)
	}
	versions := m.requirementVersions[requirementKey]
	if requirement.CurrentVersion > len(versions) || !versions[requirement.CurrentVersion-1].Confirmed {
		return monitor.Drift{}, fmt.Errorf("%w: requirement %s current version is not confirmed", monitor.ErrRequirementIDInvalid, drift.RequirementID)
	}
	for _, existing := range versions {
		if existing.OriginDriftID == drift.ID && existing.Origin == core.RequirementOriginDriftAmendment && !existing.Confirmed && !existing.Retired {
			m.monitorDrift[key] = drift
			return drift, nil
		}
	}
	current := versions[requirement.CurrentVersion-1]
	proposal, err := DriftAmendmentVersion(drift, current)
	if err != nil {
		return monitor.Drift{}, err
	}
	var issued []string
	for _, existing := range versions {
		for _, statement := range existing.Statements {
			issued = append(issued, core.RequirementStatementIDs(statement)...)
		}
	}
	if err = core.ValidateRequirementRevision(requirement.StatementHighWaterMark, issued, proposal.Statements); err != nil {
		return monitor.Drift{}, err
	}
	proposal.Workspace, proposal.RequirementID = workspace, requirement.ID
	proposal.Version, proposal.CreatedAt = len(versions)+1, now
	m.requirementVersions[requirementKey] = append(versions, proposal)
	requirement.UpdatedAt = now
	m.requirements[requirementKey] = requirement
	m.monitorDrift[key] = drift
	m.appendEventLocked(ctx, core.Event{Kind: "requirement.version_proposed", At: now, Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "requirement_id": requirement.ID,
		"version": proposal.Version, "origin": proposal.Origin,
		"origin_drift_id": proposal.OriginDriftID, "statement_count": len(proposal.Statements),
	})})
	return drift, nil
}

// reconcileConfirmedRequirementDriftLocked runs inside requirement
// confirmation. It closes only the unresolved drift in the same workspace and
// requirement that the confirmed drift-amendment version names, recording the
// requirements_amended outcome with the confirmed version and operator
// (DEC-46; req-delivery-and-forge AC-4.2). A drift another audited outcome
// already closed stays as recorded.
func (m *memory) reconcileConfirmedRequirementDriftLocked(ctx context.Context, workspace, requirementID string, confirmed core.RequirementVersion, resolvedAt time.Time) {
	if confirmed.Origin != core.RequirementOriginDriftAmendment || confirmed.OriginDriftID == "" {
		return
	}
	key := monitorKey(workspace, confirmed.OriginDriftID)
	drift, ok := m.monitorDrift[key]
	if !ok || !drift.ResolvedAt.IsZero() || drift.RequirementID != requirementID {
		return
	}
	drift.Outcome, drift.ResolvedAt = "requirements_amended", resolvedAt
	m.monitorDrift[key] = drift
	m.appendEventLocked(ctx, core.Event{TaskID: drift.TaskID, Kind: "monitor.drift_reconciled", At: resolvedAt, Payload: core.JSONPayload(
		RequirementDriftReconciledPayload(drift, confirmed))})
	if drift.SystemDesignID != "" {
		m.appendEventLocked(ctx, core.Event{Kind: "system_design.drift_resolved", At: resolvedAt, Payload: core.JSONPayload(
			RequirementDesignDriftResolvedPayload(workspace, drift, confirmed))})
	}
}

// RequirementDriftReconciledPayload is the monitor.drift_reconciled payload
// that confirmation of a drift amendment records on every backend.
func RequirementDriftReconciledPayload(drift monitor.Drift, confirmed core.RequirementVersion) map[string]any {
	return map[string]any{
		"drift_id": drift.ID, "outcome": "requirements_amended", "resolved_at": drift.ResolvedAt,
		"requirement_id": drift.RequirementID, "confirmed_version": confirmed.Version, "confirmed_by": confirmed.ConfirmedBy,
	}
}

// RequirementDesignDriftResolvedPayload is the system_design.drift_resolved
// payload for a document drift closed by a confirmed requirement amendment.
func RequirementDesignDriftResolvedPayload(workspace string, drift monitor.Drift, confirmed core.RequirementVersion) map[string]any {
	return map[string]any{
		"workspace_id": workspace, "document_id": drift.SystemDesignID, "drift_id": drift.ID,
		"outcome": "requirements_amended", "resolved_at": drift.ResolvedAt,
		"requirement_id": drift.RequirementID, "confirmed_version": confirmed.Version, "confirmed_by": confirmed.ConfirmedBy,
	}
}

// DriftAmendmentVersion carries the current confirmed requirement forward and
// inserts the out-of-pipeline change context before its canonical statement
// fence. It is a proposal only; existing confirmation remains authoritative.
func DriftAmendmentVersion(drift monitor.Drift, current core.RequirementVersion) (core.RequirementVersion, error) {
	marker := "```conveyor:requirements"
	index := strings.Index(current.Content, marker)
	if index < 0 {
		return core.RequirementVersion{}, fmt.Errorf("requirement %s current version %d has no conveyor:requirements fence", current.RequirementID, current.Version)
	}
	change := fmt.Sprintf("## Out-of-pipeline drift amendment\n\nDetected `%s` in `%s`. Source: %s.", drift.Kind, drift.Repository, drift.SourceURL)
	if drift.CommitSHA != "" {
		change += fmt.Sprintf(" Commit: `%s`.", drift.CommitSHA)
	}
	content := strings.TrimSpace(current.Content[:index]) + "\n\n" + change + "\n\n" + current.Content[index:]
	proposal := core.RequirementVersion{
		RequirementID: current.RequirementID, Content: content,
		Statements: append([]core.RequirementStatement(nil), current.Statements...),
		Origin:     core.RequirementOriginDriftAmendment, OriginDriftID: drift.ID,
	}
	if err := NormalizeRequirementVersionDocument(&proposal); err != nil {
		return core.RequirementVersion{}, err
	}
	return proposal, nil
}

func (m *memory) MonitorStatus(ctx context.Context, enabled bool, now time.Time) (monitor.Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.Status{}, ErrWorkspaceRequired
	}
	status := monitor.Status{
		WorkspaceID: workspace, Enabled: enabled,
		LastSuccessfulAt:   m.monitorLastSuccess[workspace],
		CurrentError:       m.monitorError[workspace],
		ForgeErrorCategory: m.monitorErrorCategory[workspace],
		BackoffUntil:       m.monitorBackoff[workspace],
	}
	status.Activity = append([]monitor.Activity(nil), m.monitorActivity[workspace]...)
	prefix := workspace + "\x00"
	for key, record := range m.monitorObservations {
		if strings.HasPrefix(key, prefix) {
			status.Observations = append(status.Observations, record)
		}
	}
	status.Drift = m.unresolvedDriftLocked(workspace)
	status.DriftCount = len(status.Drift)
	for _, drift := range status.Drift {
		if age := now.Sub(drift.DetectedAt); age > status.OldestDriftAge {
			status.OldestDriftAge = age
		}
	}
	sort.Slice(status.Observations, func(i, j int) bool {
		if status.Observations[i].CreatedAt.Equal(status.Observations[j].CreatedAt) {
			return status.Observations[i].Identity() < status.Observations[j].Identity()
		}
		return status.Observations[i].CreatedAt.Before(status.Observations[j].CreatedAt)
	})
	return status, nil
}

func (m *memory) ListUnresolvedDrift(ctx context.Context) ([]monitor.Drift, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return nil, ErrWorkspaceRequired
	}
	return m.unresolvedDriftLocked(workspace), nil
}

// Caller holds the memory store read lock.
func (m *memory) unresolvedDriftLocked(workspace string) []monitor.Drift {
	var result []monitor.Drift
	prefix := workspace + "\x00"
	for key, drift := range m.monitorDrift {
		if strings.HasPrefix(key, prefix) && drift.ResolvedAt.IsZero() {
			result = append(result, drift)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DetectedAt.Equal(result[j].DetectedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].DetectedAt.Before(result[j].DetectedAt)
	})
	return result
}

func (m *memory) ListActiveSystemDesignDriftCounts(ctx context.Context) (map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return nil, ErrWorkspaceRequired
	}
	counts := map[string]int{}
	prefix := workspace + "\x00"
	for key, drift := range m.monitorDrift {
		if strings.HasPrefix(key, prefix) && drift.ResolvedAt.IsZero() && drift.SystemDesignID != "" {
			counts[drift.SystemDesignID]++
		}
	}
	return counts, nil
}

func (m *memory) RecordMonitorSuccess(ctx context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	if m.monitorLastSuccess == nil {
		m.monitorLastSuccess, m.monitorError, m.monitorErrorCategory, m.monitorBackoff =
			map[string]time.Time{}, map[string]string{}, map[string]string{}, map[string]time.Time{}
	}
	m.monitorLastSuccess[workspace] = at
	delete(m.monitorError, workspace)
	delete(m.monitorErrorCategory, workspace)
	delete(m.monitorBackoff, workspace)
	return nil
}

func (m *memory) RecordMonitorFailure(ctx context.Context, category, detail string, backoffUntil time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	if m.monitorError == nil {
		m.monitorLastSuccess, m.monitorError, m.monitorErrorCategory, m.monitorBackoff =
			map[string]time.Time{}, map[string]string{}, map[string]string{}, map[string]time.Time{}
	}
	m.monitorError[workspace], m.monitorErrorCategory[workspace], m.monitorBackoff[workspace] = detail, category, backoffUntil
	return nil
}
