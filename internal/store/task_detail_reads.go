package store

import (
	"context"
)

// TaskDetailReadStore serves the request-scoped task detail with one set read
// per selection instead of one read per document
// (component-persistence, Task-detail batch reads; component-http-api,
// Request-scoped detail reads). Each method reads the workspace from the
// context, and an empty selection returns an empty result without a query.
// Any error returns no result; the volatile backend refuses a done context.
type TaskDetailReadStore interface {
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

func (m *memory) ListDocumentMetadata(ctx context.Context, ids []string) (DocumentMetadataSet, error) {
	if err := ctx.Err(); err != nil {
		return DocumentMetadataSet{}, err
	}
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
