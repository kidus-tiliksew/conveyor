package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) CreateReferenceDocument(ctx context.Context, document core.ReferenceDocument, version core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	if document.ID == "" {
		return document, version, fmt.Errorf("reference document id and name are required")
	}
	if err := core.ValidateReferenceDocumentName(document.Name); err != nil {
		return document, version, err
	}
	now := time.Now().UTC()
	document.Workspace, document.CurrentVersion = workspace(ctx), 1
	document.CreatedAt, document.UpdatedAt = now, now
	version.Workspace, version.DocumentID, version.Version = workspace(ctx), document.ID, 1
	version.CreatedBy, version.CreatedAt = store.ActorFromContext(ctx).ID, now
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO reference_documents (workspace_id,id,name,current_version,created_at,updated_at) VALUES ($1,$2,$3,1,$4,$4)`, workspace(ctx), document.ID, document.Name, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO reference_document_versions (workspace_id,document_id,version,filename,content_type,content,created_by,created_at) VALUES ($1,$2,1,$3,$4,$5,$6,$7)`, workspace(ctx), document.ID, version.Filename, version.ContentType, version.Content, version.CreatedBy, now); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "reference_document.created", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": document.ID, "version": 1, "name": document.Name})})
	})
	return document, version, err
}

func (s *Store) SupersedeReferenceDocument(ctx context.Context, documentID string, version core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error) {
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current int
		if err := tx.QueryRow(ctx, `SELECT current_version FROM reference_documents WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, workspace(ctx), documentID).Scan(&current); err != nil {
			return notFound(err, "reference document %s", documentID)
		}
		now := time.Now().UTC()
		version.Workspace, version.DocumentID = workspace(ctx), documentID
		version.Version, version.SupersedesVersion = current+1, current
		version.CreatedBy, version.CreatedAt = store.ActorFromContext(ctx).ID, now
		if _, err := tx.Exec(ctx, `INSERT INTO reference_document_versions (workspace_id,document_id,version,filename,content_type,content,supersedes_version,created_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, workspace(ctx), documentID, version.Version, version.Filename, version.ContentType, version.Content, current, version.CreatedBy, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE reference_documents SET current_version=$3,updated_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID, version.Version, now); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "reference_document.superseded", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": documentID, "version": version.Version, "supersedes_version": current})})
	})
	return version, err
}

func (s *Store) ListReferenceDocuments(ctx context.Context, includeDeleted bool) ([]core.ReferenceDocument, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,current_version,deleted_at,created_at,updated_at FROM reference_documents WHERE workspace_id=$1 AND ($2 OR deleted_at IS NULL) ORDER BY name`, workspace(ctx), includeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReferenceDocument{}
	for rows.Next() {
		var item core.ReferenceDocument
		var deleted *time.Time
		item.Workspace = workspace(ctx)
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
	item := core.ReferenceDocument{Workspace: workspace(ctx), ID: documentID}
	var deleted *time.Time
	err := s.pool.QueryRow(ctx, `SELECT name,current_version,deleted_at,created_at,updated_at FROM reference_documents WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID).
		Scan(&item.Name, &item.CurrentVersion, &deleted, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, fmt.Errorf("%w: reference document %s", store.ErrNotFound, documentID)
	}
	if deleted != nil {
		item.DeletedAt = *deleted
	}
	return item, err
}

func (s *Store) ListReferenceDocumentVersions(ctx context.Context, documentID string) ([]core.ReferenceDocumentVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT version,filename,content_type,content,coalesce(supersedes_version,0),created_by,created_at FROM reference_document_versions WHERE workspace_id=$1 AND document_id=$2 ORDER BY version`, workspace(ctx), documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReferenceDocumentVersion{}
	for rows.Next() {
		item := core.ReferenceDocumentVersion{Workspace: workspace(ctx), DocumentID: documentID}
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
	item := core.ReferenceDocumentVersion{Workspace: workspace(ctx), DocumentID: documentID}
	var supersedes *int
	err := s.pool.QueryRow(ctx, `SELECT version,filename,content_type,content,supersedes_version,created_by,created_at FROM reference_document_versions WHERE workspace_id=$1 AND document_id=$2 AND version=$3`, workspace(ctx), documentID, version).Scan(&item.Version, &item.Filename, &item.ContentType, &item.Content, &supersedes, &item.CreatedBy, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, fmt.Errorf("%w: reference document %s version %d", store.ErrNotFound, documentID, version)
	}
	if supersedes != nil {
		item.SupersedesVersion = *supersedes
	}
	return item, err
}

func (s *Store) ListReferenceDocumentEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id
		FROM events WHERE workspace_id=$1 AND kind LIKE 'reference_document.%' ORDER BY id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []core.Event{}
	for rows.Next() {
		var row db.Event
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.PayloadJson, &row.At, &row.WorkspaceID); err != nil {
			return nil, err
		}
		event := eventFromDB(row)
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil && payload["document_id"] == documentID {
			result = append(result, event)
		}
	}
	return result, rows.Err()
}

func (s *Store) DeleteReferenceDocument(ctx context.Context, documentID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var currentVersion int
		var deleted *time.Time
		if err := tx.QueryRow(ctx, `SELECT current_version,deleted_at FROM reference_documents WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), documentID).Scan(&currentVersion, &deleted); err != nil {
			return notFound(err, "reference document %s", documentID)
		}
		if deleted != nil {
			return nil
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE reference_documents SET deleted_at=$3,updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID, now); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "reference_document.deleted", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": documentID, "version": currentVersion})})
	})
}

func (s *Store) RecordReferenceDocumentConsulted(ctx context.Context, documentID string, version int, sessionID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reference_document_versions WHERE workspace_id=$1 AND document_id=$2 AND version=$3) AND EXISTS(SELECT 1 FROM planning_sessions WHERE workspace_id=$1 AND id=$4)`, workspace(ctx), documentID, version, sessionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: reference document consultation target", store.ErrNotFound)
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "reference_document.consulted", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": documentID, "version": version, "session_id": sessionID})})
	})
}
