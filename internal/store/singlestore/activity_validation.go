package singlestore

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
	// deadlines holds the quoted datetime and timestamp columns of
	// work_orders.
	deadlines []string
}

// ReadActivityValidation reads the Board validation state in one UNION ALL
// statement (component-persistence, Board activity validation reads).
func (s *Store) ReadActivityValidation(ctx context.Context, now time.Time) (store.ActivityValidation, error) {
	schema, err := s.activityValidationSchema(ctx)
	if err != nil {
		return store.ActivityValidation{}, err
	}
	query, args := activityValidationSQL(schema, documentWorkspace(ctx), now.UTC())
	rows, err := documentRows(ctx, s.db, query, args...)
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

func activityValidationSQL(schema *activityValidationSchema, workspace string, now time.Time) (string, []any) {
	parts := []string{
		"SELECT -2, 'events', COUNT(*), CAST(COALESCE(MAX(id), 0) AS CHAR) FROM events WHERE workspace_id=?",
	}
	args := []any{workspace}
	passed := make([]string, 0, len(schema.deadlines))
	var deadlineArgs []any
	for _, column := range schema.deadlines {
		passed = append(passed, "(CASE WHEN "+column+" <= ? THEN 1 ELSE 0 END)")
		deadlineArgs = append(deadlineArgs, now)
	}
	if len(passed) == 0 {
		passed = append(passed, "0")
	}
	parts = append(parts, "SELECT -1, 'deadlines', CAST(COALESCE(SUM("+strings.Join(passed, "+")+"), 0) AS SIGNED), '' FROM work_orders WHERE workspace_id=?")
	args = append(append(args, deadlineArgs...), workspace)
	for i, table := range store.ActivityValidationTables {
		values := make([]string, 0, len(schema.hashed[i]))
		for _, column := range schema.hashed[i] {
			values = append(values, "IFNULL(CONCAT('v', CAST("+column+" AS CHAR)), 'n')")
		}
		scope, scopeArgs := activityValidationScope(table.Scope, workspace)
		parts = append(parts, fmt.Sprintf("SELECT %d, '%s', COUNT(*), CAST(COALESCE(SUM(CAST(CONV(LEFT(SHA1(CONCAT_WS(CHAR(31), %s)), 15), 16, 10) AS UNSIGNED)), 0) AS CHAR) FROM `%s` WHERE %s",
			i, table.Name, strings.Join(values, ", "), table.Name, scope))
		args = append(args, scopeArgs...)
	}
	return strings.Join(parts, "\nUNION ALL "), args
}

func activityValidationScope(scope store.ActivityValidationScope, workspace string) (string, []any) {
	switch scope {
	case store.ActivityScopeTask:
		return "workspace_id=? AND task_id IN (SELECT id FROM tasks WHERE workspace_id=?)", []any{workspace, workspace}
	case store.ActivityScopeMember:
		return "id IN (SELECT user_id FROM workspace_role_bindings WHERE workspace_id=? UNION SELECT assignee_user_id FROM tasks WHERE workspace_id=? AND assignee_user_id IS NOT NULL)", []any{workspace, workspace}
	default:
		return "workspace_id=?", []any{workspace}
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
	query := "SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name IN (" + taskMapMarks(len(names)) + ") ORDER BY table_name, ordinal_position"
	rows, err := documentRows(ctx, s.db, query, taskMapArgs(names)...)
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
		if table == "work_orders" && (strings.EqualFold(dataType, "datetime") || strings.EqualFold(dataType, "timestamp")) {
			deadlines = append(deadlines, "`"+column+"`")
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
				hashed = append(hashed, "`"+column+"`")
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

// activityWorkOrdersAt is ListWorkOrdersForTasks projected at now. A whole
// workspace read keeps ListWorkOrders.
func (s *Store) activityWorkOrdersAt(ctx context.Context, taskIDs []string, now time.Time) ([]core.WorkOrder, error) {
	if len(taskIDs) == 0 {
		return s.ListWorkOrders(ctx)
	}
	orders, err := s.listOrderColumns(ctx, s.db, activityWorkOrderColumns, taskIDs)
	if err != nil {
		return nil, err
	}
	for i := range orders {
		orders[i] = store.ProjectWorkOrderAt(orders[i], now)
	}
	return orders, nil
}
