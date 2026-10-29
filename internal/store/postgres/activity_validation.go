package postgres

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// activityValidationColumns caches each store's discovered column lists. The
// schema changes only through migrations at startup, so one successful
// discovery serves the store's lifetime.
var activityValidationColumns sync.Map // *Store -> *activityValidationSchema

type activityValidationSchema struct {
	// hashed holds, per ActivityValidationTables entry, the quoted columns
	// hashed for that table.
	hashed [][]string
	// deadlines holds the quoted timestamp columns of work_orders.
	deadlines []string
}

// ReadActivityValidation reads the Board validation state in one UNION ALL
// statement, so every row comes from one snapshot (component-persistence,
// Board activity validation reads).
func (s *Store) ReadActivityValidation(ctx context.Context, now time.Time) (store.ActivityValidation, error) {
	schema, err := s.activityValidationSchema(ctx)
	if err != nil {
		return store.ActivityValidation{}, err
	}
	rows, err := s.boundary.Query(ctx, activityValidationSQL(schema), workspace(ctx), now)
	if err != nil {
		return store.ActivityValidation{}, err
	}
	defer rows.Close()
	tables := make([]store.ActivityTableSignature, len(store.ActivityValidationTables))
	var out store.ActivityValidation
	for rows.Next() {
		var ordinal int
		var name, value string
		var count int64
		if err = rows.Scan(&ordinal, &name, &count, &value); err != nil {
			return store.ActivityValidation{}, err
		}
		switch {
		case ordinal == -2:
			out.EventCount = count
			if out.EventMaxID, err = strconv.ParseInt(value, 10, 64); err != nil {
				return store.ActivityValidation{}, fmt.Errorf("activity validation event frontier: %w", err)
			}
		case ordinal == -1:
			out.PassedDeadlines = count
		case ordinal >= 0 && ordinal < len(tables):
			tables[ordinal] = store.ActivityTableSignature{Table: name, Rows: count, Sum: value}
		default:
			return store.ActivityValidation{}, fmt.Errorf("activity validation row %d", ordinal)
		}
	}
	if err = rows.Err(); err != nil {
		return store.ActivityValidation{}, err
	}
	out.Signature = store.FoldActivityValidationTables(tables)
	return out, nil
}

func activityValidationSQL(schema *activityValidationSchema) string {
	parts := []string{
		`SELECT -2, 'events'::text, count(*)::bigint, coalesce(max(id), 0)::text FROM events WHERE workspace_id=$1`,
	}
	passed := make([]string, 0, len(schema.deadlines))
	for _, column := range schema.deadlines {
		passed = append(passed, "(CASE WHEN "+column+" <= $2 THEN 1 ELSE 0 END)")
	}
	if len(passed) == 0 {
		passed = append(passed, "0")
	}
	parts = append(parts, `SELECT -1, 'deadlines'::text, coalesce(sum(`+strings.Join(passed, "+")+`), 0)::bigint, ''::text FROM work_orders WHERE workspace_id=$1`)
	for i, table := range store.ActivityValidationTables {
		parts = append(parts, fmt.Sprintf(`SELECT %d, %s::text, count(*)::bigint, coalesce(sum(hashtextextended(ROW(%s)::text, 0)), 0)::text FROM %s WHERE %s`,
			i, activityQuoteLiteral(table.Name), strings.Join(schema.hashed[i], ","), activityQuoteIdentifier(table.Name), activityValidationScope(table.Scope)))
	}
	return strings.Join(parts, "\nUNION ALL ")
}

func activityValidationScope(scope store.ActivityValidationScope) string {
	switch scope {
	case store.ActivityScopeTask:
		return `task_id IN (SELECT id FROM tasks WHERE workspace_id=$1)`
	case store.ActivityScopeMember:
		return `id IN (SELECT user_id FROM workspace_role_bindings WHERE workspace_id=$1 UNION SELECT assignee_user_id FROM tasks WHERE workspace_id=$1 AND assignee_user_id IS NOT NULL)`
	default:
		return `workspace_id=$1`
	}
}

// activityValidationSchema discovers the hashed and deadline columns from
// information_schema once per store.
func (s *Store) activityValidationSchema(ctx context.Context) (*activityValidationSchema, error) {
	if cached, ok := activityValidationColumns.Load(s); ok {
		return cached.(*activityValidationSchema), nil
	}
	names := make([]string, 0, len(store.ActivityValidationTables))
	for _, table := range store.ActivityValidationTables {
		names = append(names, table.Name)
	}
	rows, err := s.boundary.Query(ctx, `SELECT table_name::text, column_name::text, data_type::text FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name=ANY($1::text[]) ORDER BY table_name, ordinal_position`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string][]string{}
	var deadlines []string
	for rows.Next() {
		var table, column, dataType string
		if err = rows.Scan(&table, &column, &dataType); err != nil {
			return nil, err
		}
		columns[table] = append(columns[table], column)
		if table == "work_orders" && strings.HasPrefix(dataType, "timestamp") {
			deadlines = append(deadlines, activityQuoteIdentifier(column))
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	schema := &activityValidationSchema{deadlines: deadlines}
	for _, table := range store.ActivityValidationTables {
		var hashed []string
		for _, column := range columns[table.Name] {
			if !slices.Contains(table.Exclude, column) {
				hashed = append(hashed, activityQuoteIdentifier(column))
			}
		}
		if len(hashed) == 0 {
			return nil, fmt.Errorf("activity validation: table %s has no hashed columns", table.Name)
		}
		schema.hashed = append(schema.hashed, hashed)
	}
	activityValidationColumns.Store(s, schema)
	return schema, nil
}

// ReadActivityInputs returns the markers and the one order selection they
// fold, both at now, and the review lease horizon.
func (s *Store) ReadActivityInputs(ctx context.Context, taskIDs []string, now time.Time) (store.ActivityInputs, error) {
	if taskIDs != nil && len(taskIDs) == 0 {
		return store.ActivityInputs{Markers: []store.ActivityMarker{}, Orders: []core.WorkOrder{}}, nil
	}
	markers, orders, horizon, err := s.listActivityMarkers(ctx, taskIDs, now)
	if err != nil {
		return store.ActivityInputs{}, err
	}
	return store.ActivityInputs{Markers: markers, Orders: orders, ValidUntil: horizon}, nil
}

// activityWorkOrdersAt is listWorkOrdersForTasks projected at now. A whole
// workspace read keeps ListWorkOrders.
func (s *Store) activityWorkOrdersAt(ctx context.Context, taskIDs []string, now time.Time) ([]core.WorkOrder, error) {
	if len(taskIDs) == 0 {
		return s.ListWorkOrders(ctx)
	}
	rows, err := s.boundary.Query(ctx, "SELECT "+activityWorkOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=ANY($2::text[]) ORDER BY created_at,id", workspace(ctx), taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]core.WorkOrder, 0)
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, store.ProjectWorkOrderAt(order, now))
	}
	return orders, rows.Err()
}

func activityQuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func activityQuoteLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}
