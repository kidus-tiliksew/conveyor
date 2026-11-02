package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Task-detail batch reads (component-persistence, Task-detail batch reads):
// one workspace-scoped set query per selection, joined to the task's own
// work orders in SQL, with one parameterized IN list for a selection. Each
// selection's rows are collected by one helper that returns no result with
// any scan or iterator error.

// detailRows is the part of documentResultSet the collection helpers read.
type detailRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func (s *Store) ListDocumentMetadata(ctx context.Context, ids []string) (store.DocumentMetadataSet, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListDocumentMetadata")
	if len(ids) == 0 {
		return store.NewDocumentMetadataSet(), nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.DocumentMetadataSet{}, err
	}
	marks := taskMapMarks(len(ids))
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	args = append(args, taskMapArgs(ids)...)
	rows, err := documentRows(ctx, s.db, `SELECT id,'requirement' AS tier,title,COALESCE(current_version,0),archived_at IS NOT NULL,superseded_by FROM requirements
WHERE workspace_id=? AND id IN (`+marks+`)
UNION ALL SELECT id,'system_design',title,COALESCE(current_version,0),archived_at IS NOT NULL,superseded_by FROM system_designs
WHERE workspace_id=? AND id IN (`+marks+`)`, args...)
	if err != nil {
		return store.DocumentMetadataSet{}, err
	}
	defer rows.Close()
	return collectDocumentMetadata(rows)
}

func collectDocumentMetadata(rows detailRows) (store.DocumentMetadataSet, error) {
	out := store.NewDocumentMetadataSet()
	for rows.Next() {
		var row store.DocumentMetadata
		if err := rows.Scan(&row.ID, &row.Tier, &row.Title, &row.CurrentVersion, &row.Archived, &row.SupersededBy); err != nil {
			return store.DocumentMetadataSet{}, err
		}
		out.Add(row)
	}
	if err := rows.Err(); err != nil {
		return store.DocumentMetadataSet{}, translateBackendConflict(err)
	}
	return out, nil
}
