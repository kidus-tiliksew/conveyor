package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (s *Store) ListDocumentOperatorNotesForTask(ctx context.Context, taskID string) ([]core.OperatorNote, error) {
	rows, err := documentRows(ctx, s.db, `SELECT requirement_id AS document_id, version, 'requirement' AS tier,
 dismissal_note, retired_at AS dismissed_at
 FROM requirement_versions
 WHERE workspace_id=? AND origin_task_id=? AND origin='implementation'
 AND retired AND dismissal_note IS NOT NULL AND dismissal_note<>''
 UNION ALL
 SELECT document_id, version, 'system_design' AS tier, dismissal_note, dismissed_at
 FROM system_design_versions
 WHERE workspace_id=? AND origin_task_id=? AND origin='implementation_deliberation'
 AND dismissed AND dismissal_note IS NOT NULL AND dismissal_note<>''
 ORDER BY tier, document_id, version`, documentWorkspace(ctx), taskID, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []core.OperatorNote
	for rows.Next() {
		var note core.OperatorNote
		if err := rows.Scan(&note.DocumentID, &note.Version, &note.Tier, &note.Note, &note.DismissedAt); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
