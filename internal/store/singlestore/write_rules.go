package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// rowWrite is the backend-local path for future aggregate mutations. Callers
// hold their aggregate lock and supply workspace_id in values and predicates.
// It deliberately supports neither raw predicates nor expressions. Aggregate
// SQL needing expressions must invoke the same rule checks within its tx.
type rowWrite struct {
	table, operation string
	values, where    map[string]any
}

func checkWrite(w rowWrite) error {
	switch w.table {
	case "events", "deployment_events", "interventions":
		if w.operation != "INSERT" {
			return fmt.Errorf("%s is append-only", w.table)
		}
	}
	if state, ok := w.values["state"]; ok {
		valid := false
		switch w.table {
		case "tasks":
			for _, candidate := range core.TaskStates() {
				if fmt.Sprint(state) == string(candidate) {
					valid = true
				}
			}
		case "work_orders":
			for _, candidate := range core.WorkOrderStates() {
				if fmt.Sprint(state) == string(candidate) {
					valid = true
				}
			}
		default:
			return nil
		}
		if !valid {
			return fmt.Errorf("invalid %s.state %q", w.table, state)
		}
	}
	return nil
}
func checkDeploymentCredential(deployment bool, existing int) error {
	if deployment && existing > 0 {
		return fmt.Errorf("deployment credential already exists")
	}
	return nil
}
func checkReferenceName(live bool, existing int) error {
	if live && existing > 0 {
		return store.ErrReferenceDocumentNameConflict
	}
	return nil
}
func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
func sortedColumns(values map[string]any) ([]string, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		if !validIdentifier(name) {
			return nil, fmt.Errorf("invalid SQL column")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// writeRow enforces the rules that SingleStore cannot express as constraints.
// Unique checks and the write share transaction-scoped lock rows.
func writeRow(ctx context.Context, tx *sql.Tx, w rowWrite) (sql.Result, error) {
	if err := checkWrite(w); err != nil {
		return nil, err
	}
	if !validIdentifier(w.table) {
		return nil, fmt.Errorf("invalid SQL table")
	}
	if w.operation != "INSERT" && w.operation != "UPDATE" && w.operation != "DELETE" {
		return nil, fmt.Errorf("invalid SQL operation")
	}
	if w.operation != "INSERT" && len(w.where) == 0 {
		return nil, fmt.Errorf("write predicate is required")
	}
	if w.table == "user_tokens" {
		value, present := w.values["deployment_credential"]
		deployment, valid := value.(bool)
		if present && !valid {
			return nil, fmt.Errorf("deployment_credential must be boolean")
		}
		if deployment {
			if w.operation == "UPDATE" {
				if id, ok := w.where["id"].(string); !ok || id == "" {
					return nil, fmt.Errorf("credential update requires one token id")
				}
			}
			if err := lockKey(ctx, tx, "one-deployment-credential"); err != nil {
				return nil, err
			}
			var count int
			id := w.values["id"]
			if id == nil {
				id = w.where["id"]
			}
			if id == nil {
				id = ""
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_tokens WHERE deployment_credential=TRUE AND id<>?`, id).Scan(&count); err != nil {
				return nil, err
			}
			if err := checkDeploymentCredential(true, count); err != nil {
				return nil, err
			}
		}
	}
	if w.table == "reference_documents" && w.operation != "DELETE" {
		// A reference writer supplies the complete name/deleted_at projection, so
		// rename and restore receive the same uniqueness check as insertion.
		name, ok := w.values["name"].(string)
		if !ok {
			return nil, fmt.Errorf("reference write requires name")
		}
		ws := w.values["workspace_id"]
		if ws == nil {
			ws = w.where["workspace_id"]
		}
		if ws == nil {
			return nil, store.ErrWorkspaceRequired
		}
		if _, ok := w.values["deleted_at"]; !ok {
			return nil, fmt.Errorf("reference write requires deleted_at")
		}
		switch w.values["deleted_at"].(type) {
		case nil, time.Time:
		default:
			return nil, fmt.Errorf("deleted_at must be a time or untyped nil")
		}
		if w.operation == "UPDATE" {
			if id, ok := w.where["id"].(string); !ok || id == "" {
				return nil, fmt.Errorf("reference update requires one document id")
			}
			if scoped, ok := w.where["workspace_id"].(string); !ok || scoped == "" || scoped != ws {
				return nil, store.ErrWorkspaceRequired
			}
		}
		if w.values["deleted_at"] == nil {
			if err := lockKey(ctx, tx, fmt.Sprintf("reference-names:%s", ws)); err != nil {
				return nil, err
			}
			id := w.values["id"]
			if id == nil {
				id = w.where["id"]
			}
			if id == nil {
				id = ""
			}
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reference_documents WHERE workspace_id=? AND LOWER(name)=LOWER(?) AND deleted_at IS NULL AND id<>?`, ws, name, id).Scan(&count); err != nil {
				return nil, err
			}
			if err := checkReferenceName(true, count); err != nil {
				return nil, err
			}
		}
	}
	names, err := sortedColumns(w.values)
	if err != nil {
		return nil, err
	}
	var args []any
	var columns, assignments, marks []string
	for _, name := range names {
		columns = append(columns, "`"+name+"`")
		assignments = append(assignments, "`"+name+"`=?")
		marks = append(marks, "?")
		args = append(args, w.values[name])
	}
	var query string
	switch w.operation {
	case "INSERT":
		if len(names) == 0 {
			return nil, fmt.Errorf("insert values required")
		}
		query = "INSERT INTO `" + w.table + "` (" + strings.Join(columns, ",") + ") VALUES (" + strings.Join(marks, ",") + ")"
	case "UPDATE":
		if len(names) == 0 {
			return nil, fmt.Errorf("update values required")
		}
		query = "UPDATE `" + w.table + "` SET " + strings.Join(assignments, ",")
	case "DELETE":
		if len(names) != 0 {
			return nil, fmt.Errorf("delete values must be empty")
		}
		query = "DELETE FROM `" + w.table + "`"
	}
	if w.operation != "INSERT" {
		names, err = sortedColumns(w.where)
		if err != nil {
			return nil, err
		}
		var conditions []string
		for _, name := range names {
			conditions = append(conditions, "`"+name+"`=?")
			args = append(args, w.where[name])
		}
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	return tx.ExecContext(ctx, query, args...)
}
