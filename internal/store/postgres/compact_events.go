package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// ListCompactEvents returns the ListEvents rows and order. The query removes
// the governance and served-requirement snapshot bodies from work_order.*
// object payloads in the database, so they never cross the connection
// (component-persistence, Compact task event reads).
func (s *Store) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCompactEvents")
	rows, err := s.queries.ListCompactEvents(ctx, db.ListCompactEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Event, len(rows))
	for i := range rows {
		result[i] = eventFromDB(rows[i])
	}
	return result, nil
}
