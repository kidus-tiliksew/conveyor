package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskDetailReadStore serves the request-scoped task detail with one set read
// per selection instead of one read per work order or document
// (component-persistence, Task-detail batch reads; component-http-api,
// Request-scoped detail reads). Each method reads the workspace from the
// context, and an empty selection returns an empty result without a query.
type TaskDetailReadStore interface {
	// ListTaskTranscriptCaptures returns every stored capture of the task's
	// work orders, keyed by work-order ID, each list in the order
	// ListWorkOrderTranscriptCaptures returns for that order. Orders without
	// a capture are absent.
	ListTaskTranscriptCaptures(ctx context.Context, taskID string) (map[string][]core.WorkOrderTranscriptCapture, error)
	// ListTaskActivitySnapshots returns the stored snapshot of each
	// requested order that belongs to the task, keyed by order ID. The
	// caller compares the attempt.
	ListTaskActivitySnapshots(ctx context.Context, taskID string, orderIDs []string) (map[string]core.WorkOrderActivitySnapshot, error)
	// ListDocumentMetadata returns each requested requirement and System
	// Design row keyed separately by tier, so a shared ID appears in both.
	ListDocumentMetadata(ctx context.Context, ids []string) (DocumentMetadataSet, error)
}

// DocumentMetadata is one document's scalar metadata: no version, content,
// history, or event. CurrentVersion is zero when none is confirmed, and
// SupersededBy is empty, never nil.
type DocumentMetadata struct {
	ID             string
	Tier           string
	Title          string
	CurrentVersion int
	Archived       bool
	SupersededBy   []string
}

// DocumentMetadataSet keys requested document metadata by tier.
type DocumentMetadataSet struct {
	Requirements map[string]DocumentMetadata
	Designs      map[string]DocumentMetadata
}

// NewDocumentMetadataSet returns an empty, non-nil set.
func NewDocumentMetadataSet() DocumentMetadataSet {
	return DocumentMetadataSet{Requirements: map[string]DocumentMetadata{}, Designs: map[string]DocumentMetadata{}}
}

// Add files one row under its tier.
func (set DocumentMetadataSet) Add(row DocumentMetadata) {
	if row.SupersededBy == nil {
		row.SupersededBy = []string{}
	}
	if row.Tier == DocumentHeadRequirement {
		set.Requirements[row.ID] = row
	} else {
		set.Designs[row.ID] = row
	}
}

func (m *memory) ListTaskTranscriptCaptures(ctx context.Context, taskID string) (map[string][]core.WorkOrderTranscriptCapture, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]core.WorkOrderTranscriptCapture{}
	selected, hasWorkspace := WorkspaceFromContext(ctx)
	if hasWorkspace && m.tasks[taskID].Workspace != selected {
		return out, nil
	}
	for id, order := range m.workOrders {
		if order.TaskID != taskID {
			continue
		}
		if captures := m.workOrderTranscriptCaptures[id]; len(captures) > 0 {
			out[id] = append([]core.WorkOrderTranscriptCapture(nil), captures...)
		}
	}
	return out, nil
}

func (m *memory) ListTaskActivitySnapshots(ctx context.Context, taskID string, orderIDs []string) (map[string]core.WorkOrderActivitySnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]core.WorkOrderActivitySnapshot{}
	if len(orderIDs) == 0 {
		return out, nil
	}
	selected, hasWorkspace := WorkspaceFromContext(ctx)
	if hasWorkspace && m.tasks[taskID].Workspace != selected {
		return out, nil
	}
	for _, id := range orderIDs {
		order, ok := m.workOrders[id]
		if !ok || order.TaskID != taskID {
			continue
		}
		if snapshot, exists := m.workOrderActivitySnapshots[id]; exists {
			out[id] = snapshot
		}
	}
	return out, nil
}

func (m *memory) ListDocumentMetadata(ctx context.Context, ids []string) (DocumentMetadataSet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := NewDocumentMetadataSet()
	workspace := workspaceOrDefault(ctx, "")
	for _, id := range ids {
		if requirement, ok := m.requirements[memoryScopedKey{workspace: workspace, id: id}]; ok {
			out.Add(DocumentMetadata{ID: id, Tier: DocumentHeadRequirement, Title: requirement.Title, CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived, SupersededBy: append([]string(nil), requirement.SupersededBy...)})
		}
		if design, ok := m.systemDesigns[memoryScopedKey{workspace: workspace, id: id}]; ok {
			out.Add(DocumentMetadata{ID: id, Tier: DocumentHeadSystemDesign, Title: design.Title, CurrentVersion: design.CurrentVersion, Archived: design.Archived, SupersededBy: append([]string(nil), design.SupersededBy...)})
		}
	}
	return out, nil
}
