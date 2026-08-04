package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) CreateReferenceDocument(ctx context.Context, document core.ReferenceDocument, version core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, document.Workspace)
	if strings.TrimSpace(document.ID) == "" || strings.TrimSpace(document.Name) == "" {
		return document, version, fmt.Errorf("reference document id and name are required")
	}
	key := memoryScopedKey{workspace: workspace, id: document.ID}
	if _, exists := m.referenceDocuments[key]; exists {
		return document, version, fmt.Errorf("reference document %s already exists", document.ID)
	}
	for existingKey, existing := range m.referenceDocuments {
		if existingKey.workspace == workspace && existing.DeletedAt.IsZero() && strings.EqualFold(existing.Name, document.Name) {
			return document, version, fmt.Errorf("reference document name %q already exists", document.Name)
		}
	}
	now := time.Now().UTC()
	document.Workspace, document.CurrentVersion, document.CreatedAt, document.UpdatedAt = workspace, 1, now, now
	version.Workspace, version.DocumentID, version.Version, version.SupersedesVersion = workspace, document.ID, 1, 0
	version.CreatedBy, version.CreatedAt = ActorFromContext(ctx).ID, now
	m.referenceDocuments[key] = document
	m.referenceDocumentVersions[key] = []core.ReferenceDocumentVersion{version}
	m.appendEventLocked(ctx, core.Event{Kind: "reference_document.created", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "document_id": document.ID, "version": 1, "name": document.Name})})
	return document, version, nil
}

func (m *memory) SupersedeReferenceDocument(ctx context.Context, documentID string, version core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspaceOrDefault(ctx, version.Workspace), id: documentID}
	document, ok := m.referenceDocuments[key]
	if !ok || !document.DeletedAt.IsZero() {
		return version, fmt.Errorf("%w: reference document %s", ErrNotFound, documentID)
	}
	now := time.Now().UTC()
	version.Workspace, version.DocumentID = key.workspace, documentID
	version.SupersedesVersion, version.Version = document.CurrentVersion, document.CurrentVersion+1
	version.CreatedBy, version.CreatedAt = ActorFromContext(ctx).ID, now
	m.referenceDocumentVersions[key] = append(m.referenceDocumentVersions[key], version)
	document.CurrentVersion, document.UpdatedAt = version.Version, now
	m.referenceDocuments[key] = document
	m.appendEventLocked(ctx, core.Event{Kind: "reference_document.superseded", Payload: core.JSONPayload(map[string]any{"workspace_id": key.workspace, "document_id": documentID, "version": version.Version, "supersedes_version": version.SupersedesVersion})})
	return version, nil
}

func (m *memory) ListReferenceDocuments(ctx context.Context, includeDeleted bool) ([]core.ReferenceDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.ReferenceDocument{}
	for key, document := range m.referenceDocuments {
		if key.workspace == workspace && (includeDeleted || document.DeletedAt.IsZero()) {
			out = append(out, document)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memory) GetReferenceDocument(ctx context.Context, documentID string) (core.ReferenceDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	document, ok := m.referenceDocuments[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}]
	if !ok {
		return core.ReferenceDocument{}, fmt.Errorf("%w: reference document %s", ErrNotFound, documentID)
	}
	return document, nil
}

func (m *memory) ListReferenceDocumentVersions(ctx context.Context, documentID string) ([]core.ReferenceDocumentVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions, ok := m.referenceDocumentVersions[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}]
	if !ok {
		return nil, fmt.Errorf("%w: reference document %s", ErrNotFound, documentID)
	}
	return append([]core.ReferenceDocumentVersion(nil), versions...), nil
}

func (m *memory) GetReferenceDocumentVersion(ctx context.Context, documentID string, version int) (core.ReferenceDocumentVersion, error) {
	versions, err := m.ListReferenceDocumentVersions(ctx, documentID)
	if err != nil {
		return core.ReferenceDocumentVersion{}, err
	}
	for _, candidate := range versions {
		if candidate.Version == version {
			return candidate, nil
		}
	}
	return core.ReferenceDocumentVersion{}, fmt.Errorf("%w: reference document %s version %d", ErrNotFound, documentID, version)
}

func (m *memory) ListReferenceDocumentEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	result := []core.Event{}
	for _, event := range m.events[""] {
		var payload struct {
			WorkspaceID string `json:"workspace_id"`
			DocumentID  string `json:"document_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkspaceID == workspace && payload.DocumentID == documentID {
			result = append(result, event)
		}
	}
	return result, nil
}

func (m *memory) DeleteReferenceDocument(ctx context.Context, documentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}
	document, ok := m.referenceDocuments[key]
	if !ok {
		return fmt.Errorf("%w: reference document %s", ErrNotFound, documentID)
	}
	if !document.DeletedAt.IsZero() {
		return nil
	}
	document.DeletedAt, document.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	m.referenceDocuments[key] = document
	m.appendEventLocked(ctx, core.Event{Kind: "reference_document.deleted", Payload: core.JSONPayload(map[string]any{"workspace_id": key.workspace, "document_id": documentID, "version": document.CurrentVersion})})
	return nil
}

func (m *memory) RecordReferenceDocumentConsulted(ctx context.Context, documentID string, version int, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: documentID}
	if _, ok := m.referenceDocuments[key]; !ok {
		return fmt.Errorf("%w: reference document %s", ErrNotFound, documentID)
	}
	versions := m.referenceDocumentVersions[key]
	foundVersion := false
	for _, candidate := range versions {
		if candidate.Version == version {
			foundVersion = true
			break
		}
	}
	if !foundVersion {
		return fmt.Errorf("%w: reference document %s version %d", ErrNotFound, documentID, version)
	}
	if _, ok := m.planningSessions[memoryScopedKey{workspace: key.workspace, id: sessionID}]; !ok {
		return fmt.Errorf("%w: planning session %s", ErrNotFound, sessionID)
	}
	m.appendEventLocked(ctx, core.Event{Kind: "reference_document.consulted", Payload: core.JSONPayload(map[string]any{"workspace_id": key.workspace, "document_id": documentID, "version": version, "session_id": sessionID})})
	return nil
}
