package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) CreateSystemDesign(ctx context.Context, document core.SystemDesign, first core.SystemDesignVersion) (core.SystemDesign, core.SystemDesignVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, document.Workspace)
	document.ID, document.Title, document.Category = strings.TrimSpace(document.ID), strings.TrimSpace(document.Title), strings.TrimSpace(document.Category)
	if document.ID == "" || document.Title == "" || document.Category == "" {
		return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("system design id, title, and category are required")
	}
	if err := core.ValidateSystemDesignID(document.ID); err != nil {
		return core.SystemDesign{}, core.SystemDesignVersion{}, err
	}
	if document.Slug == "" {
		document.Slug = core.RequirementSlug(document.Title)
	}
	key := memoryScopedKey{workspace: workspace, id: document.ID}
	if _, exists := m.systemDesigns[key]; exists {
		return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", ErrSystemDesignIDConflict, document.ID)
	}
	for existingKey, existing := range m.systemDesigns {
		if existingKey.workspace == workspace && existing.Slug == document.Slug {
			return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", ErrSystemDesignSlugConflict, document.Slug)
		}
	}
	if err := core.NormalizeSystemDesignVersion(&first); err != nil {
		return core.SystemDesign{}, core.SystemDesignVersion{}, err
	}
	now := time.Now().UTC()
	document.Workspace, document.CurrentVersion, document.UpdatedAt = workspace, 0, now
	if document.CreatedAt.IsZero() {
		document.CreatedAt = now
	}
	first.Workspace, first.DocumentID, first.Version = workspace, document.ID, 1
	first.Confirmed, first.ConfirmedBy, first.ConfirmedAt = false, "", time.Time{}
	first.Dismissed, first.DismissedBy, first.DismissedAt = false, "", time.Time{}
	if first.CreatedAt.IsZero() {
		first.CreatedAt = now
	}
	m.systemDesigns[key], m.systemDesignVersions[key] = document, []core.SystemDesignVersion{first}
	m.appendEventLocked(ctx, core.Event{Kind: "system_design.created", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "document_id": document.ID, "title": document.Title, "category": document.Category,
	})})
	m.appendSystemDesignProposalEventLocked(ctx, first)
	return document, first, nil
}

func (m *memory) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.systemDesigns[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.SystemDesign{}, fmt.Errorf("%w: system design %s", ErrNotFound, id)
	}
	return item, nil
}

func (m *memory) ListSystemDesigns(ctx context.Context) ([]core.SystemDesign, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.SystemDesign{}
	for key, item := range m.systemDesigns {
		if key.workspace == workspace {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memory) ProposeSystemDesignVersion(ctx context.Context, version core.SystemDesignVersion) (core.SystemDesignVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, version.Workspace)
	key := memoryScopedKey{workspace: workspace, id: version.DocumentID}
	document, ok := m.systemDesigns[key]
	if !ok {
		return core.SystemDesignVersion{}, fmt.Errorf("%w: system design %s", ErrNotFound, version.DocumentID)
	}
	if err := core.NormalizeSystemDesignVersion(&version); err != nil {
		return core.SystemDesignVersion{}, err
	}
	versions := m.systemDesignVersions[key]
	version.Workspace, version.Version, version.Confirmed = workspace, len(versions)+1, false
	version.ConfirmedBy, version.ConfirmedAt = "", time.Time{}
	version.Dismissed, version.DismissedBy, version.DismissedAt = false, "", time.Time{}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now().UTC()
	}
	m.systemDesignVersions[key] = append(versions, version)
	document.UpdatedAt = version.CreatedAt
	m.systemDesigns[key] = document
	m.appendSystemDesignProposalEventLocked(ctx, version)
	return version, nil
}

func (m *memory) appendSystemDesignProposalEventLocked(ctx context.Context, version core.SystemDesignVersion) {
	m.appendEventLocked(ctx, core.Event{Kind: "system_design.version_proposed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": version.Workspace, "document_id": version.DocumentID, "version": version.Version,
		"origin": version.Origin, "origin_session_id": version.OriginSessionID, "origin_task_id": version.OriginTaskID, "governs": version.Governs,
	})})
}

func (m *memory) ConfirmSystemDesignVersion(ctx context.Context, documentID string, version int, expectedCurrentVersion ...int) (core.SystemDesign, core.SystemDesignVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: documentID}
	document, ok := m.systemDesigns[key]
	if !ok {
		return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: system design %s", ErrNotFound, documentID)
	}
	if len(expectedCurrentVersion) > 1 {
		return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("at most one expected current system design version may be supplied")
	}
	if len(expectedCurrentVersion) == 1 && expectedCurrentVersion[0] != document.CurrentVersion {
		expected := expectedCurrentVersion[0]
		return core.SystemDesign{}, core.SystemDesignVersion{}, &SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: document.CurrentVersion, Expected: &expected}
	}
	versions := m.systemDesignVersions[key]
	if version < 1 || version > len(versions) {
		return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: system design %s has no version %d", ErrNotFound, documentID, version)
	}
	confirmed := versions[version-1]
	if confirmed.Dismissed {
		return core.SystemDesign{}, core.SystemDesignVersion{}, &SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: document.CurrentVersion}
	}
	if confirmed.Confirmed && document.CurrentVersion == version {
		return document, confirmed, nil
	}
	if version < document.CurrentVersion {
		return core.SystemDesign{}, core.SystemDesignVersion{}, &SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: document.CurrentVersion}
	}
	if err := core.NormalizeSystemDesignVersion(&confirmed); err != nil {
		return core.SystemDesign{}, core.SystemDesignVersion{}, err
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	dismissed := make([]int, 0)
	for index := range versions {
		if versions[index].Version < version && !versions[index].Confirmed && !versions[index].Dismissed {
			versions[index].Dismissed, versions[index].DismissedBy, versions[index].DismissedAt = true, actor.ID, now
			dismissed = append(dismissed, versions[index].Version)
		}
	}
	confirmed.Confirmed, confirmed.ConfirmedBy, confirmed.ConfirmedAt = true, actor.ID, now
	versions[version-1] = confirmed
	m.systemDesignVersions[key] = versions
	predecessor := document.CurrentVersion
	document.CurrentVersion, document.UpdatedAt = version, now
	m.systemDesigns[key] = document
	for _, dismissedVersion := range dismissed {
		m.appendEventLocked(ctx, core.Event{Kind: "system_design.version_dismissed", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace, "document_id": documentID, "version": dismissedVersion,
			"dismissed_by": actor.ID, "confirmed_version": version,
		})})
	}
	m.appendEventLocked(ctx, core.Event{Kind: "system_design.version_confirmed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "document_id": documentID, "version": version, "supersedes_version": predecessor,
		"confirmed_by": actor.ID, "origin": confirmed.Origin, "origin_session_id": confirmed.OriginSessionID,
		"origin_task_id": confirmed.OriginTaskID, "governs": confirmed.Governs,
	})})
	return document, confirmed, nil
}

func (m *memory) GetSystemDesignVersion(ctx context.Context, documentID string, version int) (core.SystemDesignVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.systemDesignVersions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}]
	if version < 1 || version > len(versions) {
		return core.SystemDesignVersion{}, fmt.Errorf("%w: system design %s has no version %d", ErrNotFound, documentID, version)
	}
	return versions[version-1], nil
}

func (m *memory) ListSystemDesignVersions(ctx context.Context, documentID string) ([]core.SystemDesignVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.systemDesignVersions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}]
	return append([]core.SystemDesignVersion(nil), versions...), nil
}

func (m *memory) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.SystemDesignVersion{}
	for key, versions := range m.systemDesignVersions {
		if key.workspace == workspace {
			out[key.id] = append([]core.SystemDesignVersion(nil), versions...)
		}
	}
	return out, nil
}

func (m *memory) ListSystemDesignEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []core.Event{}
	workspace := workspaceOrDefault(ctx, "")
	for _, event := range m.events[""] {
		var payload map[string]any
		if strings.HasPrefix(event.Kind, "system_design.") && json.Unmarshal(event.Payload, &payload) == nil &&
			payload["workspace_id"] == workspace && payload["document_id"] == documentID {
			out = append(out, event)
		}
	}
	return out, nil
}

func (m *memory) ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]core.Event{}
	workspace := workspaceOrDefault(ctx, "")
	for _, event := range m.events[""] {
		var payload map[string]any
		if !strings.HasPrefix(event.Kind, "system_design.") || json.Unmarshal(event.Payload, &payload) != nil || payload["workspace_id"] != workspace {
			continue
		}
		if documentID, _ := payload["document_id"].(string); documentID != "" {
			out[documentID] = append(out[documentID], event)
		}
	}
	return out, nil
}

func (m *memory) RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: documentID}
	found := false
	for _, candidate := range m.systemDesignVersions[key] {
		if candidate.Version == version {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: system design %s version %d", ErrNotFound, documentID, version)
	}
	if workOrderID != "" {
		if _, ok := m.workOrders[workOrderID]; !ok {
			return fmt.Errorf("%w: work order %s", ErrNotFound, workOrderID)
		}
	} else if _, ok := m.planningSessions[memoryScopedKey{workspace: workspace, id: sessionID}]; !ok {
		return fmt.Errorf("%w: planning session %s", ErrNotFound, sessionID)
	}
	m.appendEventLocked(ctx, core.Event{Kind: "system_design.consulted", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "document_id": documentID, "version": version,
		"session_id": sessionID, "work_order_id": workOrderID,
	})})
	return nil
}

func (m *memory) ProposeDecision(ctx context.Context, decision core.Decision) (core.Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, decision.Workspace)
	if decision.ID == "" {
		high := 0
		for key, item := range m.decisions {
			if key.workspace == workspace {
				n, _ := strconv.Atoi(strings.TrimPrefix(item.ID, "DEC-"))
				if n > high {
					high = n
				}
			}
		}
		decision.ID = fmt.Sprintf("DEC-%d", high+1)
	}
	if err := core.ValidateDecision(decision); err != nil {
		return core.Decision{}, err
	}
	key := memoryScopedKey{workspace: workspace, id: decision.ID}
	if _, exists := m.decisions[key]; exists {
		return core.Decision{}, fmt.Errorf("%w: %s", ErrDecisionIDConflict, decision.ID)
	}
	if decision.Supersedes != "" {
		predecessor, ok := m.decisions[memoryScopedKey{workspace: workspace, id: decision.Supersedes}]
		if !ok || predecessor.Status != core.DecisionConfirmed {
			return core.Decision{}, fmt.Errorf("%w: decision %s can supersede only a confirmed decision", ErrDecisionSupersessionConflict, decision.ID)
		}
	}
	decision.Workspace, decision.Status = workspace, core.DecisionProposed
	decision.ConfirmedBy, decision.ConfirmedAt, decision.DismissedBy, decision.DismissedAt, decision.SupersededBy = "", time.Time{}, "", time.Time{}, ""
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now().UTC()
	}
	m.decisions[key] = decision
	m.appendEventLocked(ctx, core.Event{Kind: "decision.proposed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "decision_id": decision.ID, "origin": decision.Origin,
		"origin_session_id": decision.OriginSessionID, "origin_task_id": decision.OriginTaskID, "supersedes": decision.Supersedes,
	})})
	return decision, nil
}

func (m *memory) DismissDecision(ctx context.Context, id string) (core.Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: id}
	decision, ok := m.decisions[key]
	if !ok {
		return core.Decision{}, fmt.Errorf("%w: decision %s", ErrNotFound, id)
	}
	if decision.Status == core.DecisionDismissed {
		return decision, nil
	}
	if decision.Status != core.DecisionProposed {
		return core.Decision{}, fmt.Errorf("%w: decision %s is %s and cannot be dismissed", ErrDecisionSupersessionConflict, id, decision.Status)
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	decision.Status, decision.DismissedBy, decision.DismissedAt = core.DecisionDismissed, actor.ID, now
	m.decisions[key] = decision
	m.appendEventLocked(ctx, core.Event{Kind: "decision.dismissed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "decision_id": id, "dismissed_by": actor.ID, "supersedes": decision.Supersedes,
	})})
	return decision, nil
}

func (m *memory) ConfirmDecision(ctx context.Context, id string) (core.Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: id}
	decision, ok := m.decisions[key]
	if !ok {
		return core.Decision{}, fmt.Errorf("%w: decision %s", ErrNotFound, id)
	}
	if decision.Status == core.DecisionConfirmed {
		return decision, nil
	}
	if decision.Status != core.DecisionProposed {
		return core.Decision{}, fmt.Errorf("%w: decision %s is %s and cannot be confirmed", ErrDecisionSupersessionConflict, id, decision.Status)
	}
	if decision.Supersedes != "" {
		priorKey := memoryScopedKey{workspace: workspace, id: decision.Supersedes}
		prior, exists := m.decisions[priorKey]
		if !exists || prior.Status != core.DecisionConfirmed || prior.SupersededBy != "" {
			return core.Decision{}, fmt.Errorf("%w: %s is no longer confirmed", ErrDecisionSupersessionConflict, decision.Supersedes)
		}
		actor, now := ActorFromContext(ctx), time.Now().UTC()
		prior.Status, prior.SupersededBy = core.DecisionSuperseded, decision.ID
		m.decisions[priorKey] = prior
		decision.Status, decision.ConfirmedBy, decision.ConfirmedAt = core.DecisionConfirmed, actor.ID, now
		m.decisions[key] = decision
	} else {
		actor, now := ActorFromContext(ctx), time.Now().UTC()
		decision.Status, decision.ConfirmedBy, decision.ConfirmedAt = core.DecisionConfirmed, actor.ID, now
		m.decisions[key] = decision
	}
	actor := ActorFromContext(ctx)
	m.appendEventLocked(ctx, core.Event{Kind: "decision.confirmed", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "decision_id": id, "confirmed_by": actor.ID, "supersedes": decision.Supersedes,
	})})
	return decision, nil
}

func (m *memory) GetDecision(ctx context.Context, id string) (core.Decision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.decisions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.Decision{}, fmt.Errorf("%w: decision %s", ErrNotFound, id)
	}
	return item, nil
}

func (m *memory) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.Decision{}
	for key, item := range m.decisions {
		if key.workspace == workspace {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return decisionOrdinal(out[i].ID) < decisionOrdinal(out[j].ID) })
	return out, nil
}

func decisionOrdinal(id string) int { n, _ := strconv.Atoi(strings.TrimPrefix(id, "DEC-")); return n }
