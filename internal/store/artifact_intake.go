package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type ArtifactUpload struct {
	Name        string
	ContentType string
	Content     []byte
}

// PrepareIntakeArtifacts validates every attachment before any task write
// (component-persistence ART-STORE-1; req-intake-and-triage REQ-1).
func PrepareIntakeArtifacts(t core.Task, uploads []ArtifactUpload) ([]core.Artifact, error) {
	result := make([]core.Artifact, len(uploads))
	for i, u := range uploads {
		media, err := core.ValidateArtifactMedia(u.ContentType, u.Content)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", u.Name, err)
		}
		result[i] = core.Artifact{Workspace: t.Workspace, TaskID: t.ID, Name: u.Name, ContentType: media, Role: core.ArtifactRoleTaskContext, CreatedAt: t.CreatedAt}
	}
	return result, nil
}

func IntakeFinalizedEvent(t core.Task) core.Event {
	return core.Event{TaskID: t.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": core.TaskClaiming, "to": core.TaskQueued, "command": core.TaskIntakeFinalize})}
}
func IntakeArtifactEvent(t core.Task, a core.Artifact) core.Event {
	return core.Event{TaskID: t.ID, Kind: "artifact.attached", Payload: core.JSONPayload(map[string]any{"artifact_id": a.ID, "content_type": a.ContentType, "name": a.Name})}
}

func (m *memory) CreateTaskWithAttachments(ctx context.Context, t core.Task, ids []string, attached TaskContextInput, uploads []ArtifactUpload) error {
	if t.Workspace != workspaceOrDefault(ctx, t.Workspace) {
		return fmt.Errorf("task workspace mismatch")
	}
	artifacts, err := PrepareIntakeArtifacts(t, uploads)
	if err != nil {
		return err
	}
	if _, err = core.TransitionTask(core.TaskClaiming, core.TaskIntakeFinalize); err != nil {
		return err
	}
	if strings.ContainsRune(ActorFromContext(ctx).ID, 0) {
		return fmt.Errorf("audit actor contains NUL")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	staged := memory{tasks: maps.Clone(m.tasks), dependencies: maps.Clone(m.dependencies), events: cloneStartOverSlices(m.events), lineage: maps.Clone(m.lineage), artifacts: maps.Clone(m.artifacts), requirements: m.requirements, systemDesigns: m.systemDesigns, planningSessions: m.planningSessions, repositoryInstalls: cloneStartOverSlices(m.repositoryInstalls), nextEventID: m.nextEventID}
	for k, a := range staged.artifacts {
		a.links = slices.Clone(a.links)
		staged.artifacts[k] = a
	}
	t.State = core.TaskClaiming
	if err = staged.createTaskWithContextLocked(ctx, t, ids, attached, nil); err != nil {
		return err
	}
	for i, a := range artifacts {
		stored, err := staged.createArtifactLocked(ctx, a, uploads[i].Content)
		if err != nil {
			return err
		}
		staged.appendEventLocked(ctx, IntakeArtifactEvent(t, stored))
	}
	t = staged.tasks[t.ID]
	t.State = core.TaskQueued
	staged.tasks[t.ID] = t
	staged.appendEventLocked(ctx, IntakeFinalizedEvent(t))
	m.tasks, m.dependencies, m.events, m.lineage, m.artifacts, m.repositoryInstalls, m.nextEventID = staged.tasks, staged.dependencies, staged.events, staged.lineage, staged.artifacts, staged.repositoryInstalls, staged.nextEventID
	return nil
}
