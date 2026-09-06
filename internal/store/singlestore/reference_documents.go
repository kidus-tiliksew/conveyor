package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) CreateReferenceDocument(ctx context.Context, document core.ReferenceDocument, version core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	if document.ID == "" {
		return document, version, fmt.Errorf("reference document id and name are required")
	}
	if err := core.ValidateReferenceDocumentName(document.Name); err != nil {
		return document, version, err
	}
	now := time.Now().UTC()
	document.Workspace, document.CurrentVersion = documentWorkspace(ctx), 1
	document.CreatedAt, document.UpdatedAt = now, now
	version.Workspace, version.DocumentID, version.Version = documentWorkspace(ctx), document.ID, 1
	version.CreatedBy, version.CreatedAt = store.ActorFromContext(ctx).ID, now
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		if _, err := writeRow(ctx, tx, rowWrite{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": documentWorkspace(ctx), "id": document.ID, "name": document.Name, "current_version": 1, "deleted_at": nil, "created_at": now, "updated_at": now}}); err != nil {
			return err
		}
		if _, err := documentExec(ctx, tx, `INSERT INTO reference_document_versions (workspace_id,document_id,version,filename,content_type,content,created_by,created_at) VALUES (?,?,1,?,?,?,?,?)`, documentWorkspace(ctx), document.ID, version.Filename, version.ContentType, version.Content, version.CreatedBy, now); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "reference_document.created", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": document.ID, "version": 1, "name": document.Name})}); err != nil {
			return err
		}
		return recomputeDecisionSweepsForDocumentTx(ctx, tx, core.DecisionSweepTierReferenceDocument, document.ID, version.Content)
	})
	return document, version, err
}

func (s *Store) SupersedeReferenceDocument(ctx context.Context, documentID string, version core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error) {
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var current int
		if err := documentRow(ctx, tx, `SELECT current_version FROM reference_documents WHERE workspace_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`, documentWorkspace(ctx), documentID).Scan(&current); err != nil {
			return notFound(err, "reference document %s", documentID)
		}
		now := time.Now().UTC()
		version.Workspace, version.DocumentID = documentWorkspace(ctx), documentID
		version.Version, version.SupersedesVersion = current+1, current
		version.CreatedBy, version.CreatedAt = store.ActorFromContext(ctx).ID, now
		if _, err := documentExec(ctx, tx, `INSERT INTO reference_document_versions (workspace_id,document_id,version,filename,content_type,content,supersedes_version,created_by,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, documentWorkspace(ctx), documentID, version.Version, version.Filename, version.ContentType, version.Content, current, version.CreatedBy, now); err != nil {
			return err
		}
		if _, err := documentExec(ctx, tx, `UPDATE reference_documents SET current_version=?,updated_at=? WHERE workspace_id=? AND id=?`, version.Version, now, documentWorkspace(ctx), documentID); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "reference_document.superseded", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": version.Version, "supersedes_version": current})}); err != nil {
			return err
		}
		return recomputeDecisionSweepsForDocumentTx(ctx, tx, core.DecisionSweepTierReferenceDocument, documentID, version.Content)
	})
	return version, err
}

func (s *Store) ListReferenceDocuments(ctx context.Context, includeDeleted bool) ([]core.ReferenceDocument, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,name,current_version,deleted_at,created_at,updated_at FROM reference_documents WHERE workspace_id=? AND (? OR deleted_at IS NULL) ORDER BY name`, documentWorkspace(ctx), includeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReferenceDocument{}
	for rows.Next() {
		var item core.ReferenceDocument
		var deleted *time.Time
		item.Workspace = documentWorkspace(ctx)
		if err = rows.Scan(&item.ID, &item.Name, &item.CurrentVersion, &deleted, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if deleted != nil {
			item.DeletedAt = *deleted
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetReferenceDocument(ctx context.Context, documentID string) (core.ReferenceDocument, error) {
	item := core.ReferenceDocument{Workspace: documentWorkspace(ctx), ID: documentID}
	var deleted *time.Time
	err := documentRow(ctx, s.db, `SELECT name,current_version,deleted_at,created_at,updated_at FROM reference_documents WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), documentID).
		Scan(&item.Name, &item.CurrentVersion, &deleted, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: reference document %s", store.ErrNotFound, documentID)
	}
	if deleted != nil {
		item.DeletedAt = *deleted
	}
	return item, err
}

func (s *Store) ListReferenceDocumentVersions(ctx context.Context, documentID string) ([]core.ReferenceDocumentVersion, error) {
	rows, err := documentRows(ctx, s.db, `SELECT version,filename,content_type,content,coalesce(supersedes_version,0),created_by,created_at FROM reference_document_versions WHERE workspace_id=? AND document_id=? ORDER BY version`, documentWorkspace(ctx), documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReferenceDocumentVersion{}
	for rows.Next() {
		item := core.ReferenceDocumentVersion{Workspace: documentWorkspace(ctx), DocumentID: documentID}
		if err = rows.Scan(&item.Version, &item.Filename, &item.ContentType, &item.Content, &item.SupersedesVersion, &item.CreatedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: reference document %s", store.ErrNotFound, documentID)
	}
	return out, rows.Err()
}

func (s *Store) GetReferenceDocumentVersion(ctx context.Context, documentID string, version int) (core.ReferenceDocumentVersion, error) {
	item := core.ReferenceDocumentVersion{Workspace: documentWorkspace(ctx), DocumentID: documentID}
	var supersedes *int
	err := documentRow(ctx, s.db, `SELECT version,filename,content_type,content,supersedes_version,created_by,created_at FROM reference_document_versions WHERE workspace_id=? AND document_id=? AND version=?`, documentWorkspace(ctx), documentID, version).Scan(&item.Version, &item.Filename, &item.ContentType, &item.Content, &supersedes, &item.CreatedBy, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: reference document %s version %d", store.ErrNotFound, documentID, version)
	}
	if supersedes != nil {
		item.SupersedesVersion = *supersedes
	}
	return item, err
}

func (s *Store) ListReferenceDocumentEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id
		FROM events WHERE workspace_id=? AND kind LIKE 'reference_document.%' ORDER BY id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []core.Event{}
	for rows.Next() {
		var row core.Event
		var eventWorkspace string
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.Payload, &row.At, &eventWorkspace); err != nil {
			return nil, err
		}
		event := row
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil && payload["document_id"] == documentID {
			result = append(result, event)
		}
	}
	return result, rows.Err()
}

func (s *Store) DeleteReferenceDocument(ctx context.Context, documentID string) error {
	return s.documentTx(ctx, func(tx *sql.Tx) error {
		var currentVersion int
		var deleted *time.Time
		if err := documentRow(ctx, tx, `SELECT current_version,deleted_at FROM reference_documents WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), documentID).Scan(&currentVersion, &deleted); err != nil {
			return notFound(err, "reference document %s", documentID)
		}
		if deleted != nil {
			return nil
		}
		now := time.Now().UTC()
		if _, err := documentExec(ctx, tx, `UPDATE reference_documents SET deleted_at=?,updated_at=? WHERE workspace_id=? AND id=?`, now, now, documentWorkspace(ctx), documentID); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "reference_document.deleted", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": currentVersion})})
	})
}

func (s *Store) RecordReferenceDocumentConsulted(ctx context.Context, documentID string, version int, sessionID string) error {
	return s.documentTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM reference_document_versions WHERE workspace_id=? AND document_id=? AND version=?) AND EXISTS(SELECT 1 FROM planning_sessions WHERE workspace_id=? AND id=?)`, documentWorkspace(ctx), documentID, version, documentWorkspace(ctx), sessionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: reference document consultation target", store.ErrNotFound)
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "reference_document.consulted", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": version, "session_id": sessionID})})
	})
}
