package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task-detail batch reads (component-persistence, Task-detail batch reads):
// one workspace-scoped set query per selection, joined to the task's own
// work orders in SQL. Each selection's rows are collected by one helper that
// returns no result with any scan or iterator error.

// detailRows is the part of pgx.Rows the collection helpers read.
type detailRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func (s *Store) ListDocumentMetadata(ctx context.Context, ids []string) (store.DocumentMetadataSet, error) {
	if len(ids) == 0 {
		return store.NewDocumentMetadataSet(), nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id, 'requirement'::text, title, COALESCE(current_version,0)::int, archived_at IS NOT NULL, superseded_by FROM requirements
WHERE workspace_id=$1 AND id=ANY($2::text[])
UNION ALL
SELECT id, 'system_design'::text, title, COALESCE(current_version,0)::int, archived_at IS NOT NULL, superseded_by FROM system_designs
WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspace(ctx), ids)
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
		return store.DocumentMetadataSet{}, err
	}
	return out, nil
}
