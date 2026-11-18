package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerSQL is the PostgreSQL spelling of the shared activity-marker fold
// read (store.MarkerFoldSQL). Snapshot bodies are not marker inputs and never
// cross the connection.
var markerSQL = store.MarkerSQLDialect{
	Lease:       store.PostgresLeaseSQL,
	Placeholder: func(n int) string { return fmt.Sprintf("$%d", n) },
	Scope: func(b *store.MarkerSQLBinder, ws string, page []string) string {
		workspace, tasks := b.Bind(ws), b.Bind(emptyIfNil(page))
		return `FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=` + workspace + `
		AND (cardinality(` + tasks + `::text[])=0 OR t.id=ANY(` + tasks + `::text[]))`
	},
	Compact: compactEventPayloadColumn,
	InList: func(b *store.MarkerSQLBinder, col string, values []string) string {
		return col + "=ANY(" + b.Bind(values) + "::text[])"
	},
	Int:  func(placeholder string) string { return placeholder + "::bigint" },
	Text: func(json string) string { return "(" + json + ")::text" },
	Keys: func(json string) string {
		return "(CASE WHEN jsonb_typeof(" + json + ")='object' THEN (SELECT COALESCE(jsonb_agg(k),'[]'::jsonb) FROM jsonb_object_keys(" + json + ") AS k)::text END)"
	},
	Field:     func(json, key string) string { return "(" + json + ")->'" + key + "'" },
	FieldText: func(json, key string) string { return "(" + json + ")->>'" + key + "'" },
	IsString:  func(value string) string { return "(COALESCE(jsonb_typeof(" + value + "),'')='string')" },
	Object:    func(pairs []string) string { return "jsonb_build_object(" + strings.Join(pairs, ",") + ")" },
	Contains:  func(text, literal string) string { return "(strpos(" + text + "," + literal + ")>0)" },
	Lower:     func(text string) string { return "lower(" + text + ")" },
	Stripped: func(text, literal string) string {
		return "octet_length(replace(" + text + "," + literal + ",''))"
	},
	ASCII: func(text string) string { return "(octet_length(" + text + ")=char_length(" + text + "))" },
}

// markerFoldRows reads the fold inputs of one page and reads back the claim
// histories whose newest claim does not supersede the claims the reduction
// left out (store.MarkerClaimBackfill).
func (s *Store) markerFoldRows(ctx context.Context, in store.MarkerFoldInput) ([]core.Event, error) {
	query, args := store.MarkerFoldSQL(markerSQL, in)
	rows, err := s.markerFoldScan(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if partitions := store.MarkerClaimBackfill(rows, in.OpenReviewOrders); len(partitions) > 0 {
		query, args = store.MarkerClaimHistorySQL(markerSQL, in, partitions)
		history, err := s.markerFoldScan(ctx, query, args)
		if err != nil {
			return nil, err
		}
		rows = store.MergeMarkerFoldRows(rows, history)
	}
	out := make([]core.Event, len(rows))
	for i := range rows {
		out[i] = rows[i].Event
	}
	return out, nil
}

func (s *Store) markerFoldScan(ctx context.Context, query string, args []any) ([]store.MarkerFoldRow, error) {
	rows, err := s.boundary.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.MarkerFoldRow{}
	for rows.Next() {
		var row store.MarkerFoldRow
		if err = rows.Scan(&row.Event.ID, &row.Event.TaskID, &row.Event.JobID, &row.Event.Kind, &row.Event.Payload, &row.Event.At, &row.Dropped, &row.DroppedImplement, &row.FutureDropped); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
