package db

// This file is hand-maintained because the repository's templated CHECK
// migrations are intentionally not parseable by sqlc outside the build-time
// validation path. Keep the SQL mirrored in queries/control_plane.sql.

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (q *Queries) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.db.QueryRow(ctx, sql, args...)
}

type InsertLineageLinkParams struct {
	WorkspaceID      string
	SrcType          string
	SrcID            string
	DstType          string
	DstID            string
	Kind             string
	CreatedByEventID int64
	CreatedAt        pgtype.Timestamptz
}

func (q *Queries) InsertLineageLink(ctx context.Context, arg InsertLineageLinkParams) error {
	_, err := q.db.Exec(ctx, `INSERT INTO links
        (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
        ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind)
        DO UPDATE SET
          created_by_event_id=LEAST(COALESCE(links.created_by_event_id,EXCLUDED.created_by_event_id),EXCLUDED.created_by_event_id),
          created_at=LEAST(links.created_at,EXCLUDED.created_at),
          legacy_created_by_event=NULL`,
		arg.WorkspaceID, arg.SrcType, arg.SrcID, arg.DstType, arg.DstID,
		arg.Kind, arg.CreatedByEventID, arg.CreatedAt)
	return err
}

func (q *Queries) DeleteLineageLinks(ctx context.Context, workspaceID string) (int64, error) {
	tag, err := q.db.Exec(ctx, `DELETE FROM links WHERE workspace_id=$1
		AND created_by_event_id IS NOT NULL AND kind = ANY(ARRAY[
		'depends_on','dispatches','materializes','merged_range','produced_blueprint',
		'produced_requirement','produced_verdict','serves','submitted_as','submitted_range',
		'supersedes','supports','versions'
	]::text[])`, workspaceID)
	return tag.RowsAffected(), err
}

func (q *Queries) ListLineageLinks(ctx context.Context, workspaceID string) ([]Link, error) {
	rows, err := q.db.Query(ctx, `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,
        legacy_created_by_event,created_at,created_by_event_id
        FROM links WHERE workspace_id=$1
        ORDER BY created_by_event_id,src_type,src_id,dst_type,dst_id,kind`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Link{}
	for rows.Next() {
		var item Link
		if err = rows.Scan(&item.WorkspaceID, &item.SrcType, &item.SrcID, &item.DstType,
			&item.DstID, &item.Kind, &item.LegacyCreatedByEvent, &item.CreatedAt,
			&item.CreatedByEventID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (q *Queries) ListWorkspaceEvents(ctx context.Context, workspaceID string) ([]Event, error) {
	rows, err := q.db.Query(ctx, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id
        FROM events WHERE workspace_id=$1 ORDER BY id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Event{}
	for rows.Next() {
		var item Event
		if err = rows.Scan(&item.ID, &item.TaskID, &item.JobID, &item.Kind, &item.ActorID,
			&item.ActorRole, &item.PayloadJson, &item.At, &item.WorkspaceID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
