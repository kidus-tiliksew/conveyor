package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task-detail batch reads (component-persistence, Task-detail batch reads):
// one workspace-scoped set query per selection, joined to the task's own
// work orders in SQL, with one parameterized IN list for a selection.

func (s *Store) ListTaskTranscriptCaptures(ctx context.Context, taskID string) (map[string][]core.WorkOrderTranscriptCapture, error) {
	rows, err := documentRows(ctx, s.db, `SELECT c.work_order_id, c.attempt_id, c.content, c.termination_reason, c.truncated, c.captured_at
		FROM work_order_transcript_captures c
		JOIN work_orders o ON o.workspace_id=c.workspace_id AND o.id=c.work_order_id
		WHERE c.workspace_id=? AND o.task_id=?
		ORDER BY c.work_order_id, c.captured_at, c.attempt_id`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.WorkOrderTranscriptCapture{}
	for rows.Next() {
		var orderID string
		var capture core.WorkOrderTranscriptCapture
		if err = rows.Scan(&orderID, &capture.AttemptID, &capture.Content, &capture.TerminationReason, &capture.Truncated, &capture.CapturedAt); err != nil {
			return nil, err
		}
		out[orderID] = append(out[orderID], capture)
	}
	return out, translateBackendConflict(rows.Err())
}

func (s *Store) ListTaskActivitySnapshots(ctx context.Context, taskID string, orderIDs []string) (map[string]core.WorkOrderActivitySnapshot, error) {
	out := map[string]core.WorkOrderActivitySnapshot{}
	if len(orderIDs) == 0 {
		return out, nil
	}
	args := append([]any{documentWorkspace(ctx), taskID}, taskMapArgs(orderIDs)...)
	rows, err := documentRows(ctx, s.db, `SELECT a.work_order_id, a.attempt_id, a.content, a.captured_at
		FROM work_order_activity_snapshots a
		JOIN work_orders o ON o.workspace_id=a.workspace_id AND o.id=a.work_order_id
		WHERE a.workspace_id=? AND o.task_id=? AND a.work_order_id IN (`+taskMapMarks(len(orderIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var orderID string
		var snapshot core.WorkOrderActivitySnapshot
		if err = rows.Scan(&orderID, &snapshot.AttemptID, &snapshot.Content, &snapshot.CapturedAt); err != nil {
			return nil, err
		}
		out[orderID] = snapshot
	}
	return out, translateBackendConflict(rows.Err())
}

func (s *Store) ListDocumentMetadata(ctx context.Context, ids []string) (store.DocumentMetadataSet, error) {
	out := store.NewDocumentMetadataSet()
	if len(ids) == 0 {
		return out, nil
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
	for rows.Next() {
		var row store.DocumentMetadata
		if err = rows.Scan(&row.ID, &row.Tier, &row.Title, &row.CurrentVersion, &row.Archived, &row.SupersededBy); err != nil {
			return store.DocumentMetadataSet{}, err
		}
		out.Add(row)
	}
	if err = translateBackendConflict(rows.Err()); err != nil {
		return store.DocumentMetadataSet{}, err
	}
	return out, nil
}
