package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ListDocumentOperatorNotesForTask(ctx context.Context, taskID string) ([]core.OperatorNote, error) {
	rows, err := db.New(s.pool).ListDocumentOperatorNotesForTask(ctx, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	var notes []core.OperatorNote
	for _, row := range rows {
		notes = append(notes, core.OperatorNote{DocumentID: row.DocumentID, Version: row.Version, Tier: row.Tier, Note: row.DismissalNote, DismissedAt: row.DismissedAt.Time})
	}
	return notes, nil
}
