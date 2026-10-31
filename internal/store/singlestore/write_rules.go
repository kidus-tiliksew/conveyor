package singlestore

import (
	"context"
	"database/sql"
	"errors"
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
	case "tasks":
		if w.operation == "UPDATE" {
			if _, ok := w.values["intake_key"]; ok {
				return fmt.Errorf("task intake_key is immutable")
			}
		}
	case "workspaces":
		switch w.operation {
		case "UPDATE":
			if _, ok := w.values["name"]; ok {
				return fmt.Errorf("workspace name is immutable")
			}
		case "DELETE":
			return fmt.Errorf("workspaces rows are retired, not deleted")
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
// A unique rule claims its value on a lock row (claimKey) and also counts the
// committed rows, which covers rows written before claims existed.
func writeRow(ctx context.Context, tx *sql.Tx, w rowWrite) (sql.Result, error) {
	if err := checkWrite(w); err != nil {
		return nil, err
	}
	// Every ledger row names the explicit context actor; a row that already
	// carries actor values does not bypass the check (component-persistence,
	// Actor context).
	if w.table == "events" || w.table == "deployment_events" {
		if _, err := store.RequireActor(ctx); err != nil {
			return nil, fmt.Errorf("%s insert: %w", w.table, err)
		}
	}
	if w.table == "events" {
		taskID, _ := w.values["task_id"].(string)
		w.values["event_shard_key"] = eventShardKey(taskID)
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
	if w.operation == "DELETE" && (w.table == "user_tokens" || w.table == "reference_documents" || w.table == "tasks") {
		// Claims on lock rows track these rows; a hard delete would orphan one
		// (DEC-70, component-persistence Lock and transaction contract).
		return nil, fmt.Errorf("%s rows are retired, not deleted", w.table)
	}
	if w.table == "user_tokens" {
		value, present := w.values["deployment_credential"]
		deployment, valid := value.(bool)
		if present && !valid {
			return nil, fmt.Errorf("deployment_credential must be boolean")
		}
		if deployment || (present && w.operation == "UPDATE") {
			id, _ := w.values["id"].(string)
			if w.operation == "UPDATE" {
				id, _ = w.where["id"].(string)
			}
			if id == "" {
				return nil, fmt.Errorf("credential write requires one token id")
			}
			holder, err := claimKey(ctx, tx, "one-deployment-credential")
			if err != nil {
				return nil, err
			}
			if deployment {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_tokens WHERE deployment_credential=TRUE AND id<>?`, id).Scan(&count); err != nil {
					return nil, err
				}
				if holder != "" && holder != id {
					count++
				}
				if err := checkDeploymentCredential(true, count); err != nil {
					return nil, err
				}
				if err := setClaim(ctx, tx, "one-deployment-credential", id); err != nil {
					return nil, err
				}
			} else if holder == id {
				if err := setClaim(ctx, tx, "one-deployment-credential", ""); err != nil {
					return nil, err
				}
			}
		}
	}
	if w.table == "reference_documents" {
		// A reference writer supplies the complete name/deleted_at projection.
		// Documents are created live and only soft-deleted afterwards, so each
		// live name has exactly one claim to take and release.
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
		var live bool
		switch w.values["deleted_at"].(type) {
		case nil:
			live = true
		case time.Time:
		default:
			return nil, fmt.Errorf("deleted_at must be a time or untyped nil")
		}
		id, _ := w.values["id"].(string)
		if w.operation == "UPDATE" {
			if live {
				return nil, fmt.Errorf("reference update must soft-delete")
			}
			id, _ = w.where["id"].(string)
			if id == "" {
				return nil, fmt.Errorf("reference update requires one document id")
			}
			if scoped, ok := w.where["workspace_id"].(string); !ok || scoped == "" || scoped != ws {
				return nil, store.ErrWorkspaceRequired
			}
		} else if !live || id == "" {
			return nil, fmt.Errorf("reference insert requires a live document id")
		}
		// The claim and the count compare the same LOWER(name) bytes. BINARY
		// drops PAD SPACE, so a trailing-space variant is distinct on both,
		// as under the Postgres lower(name) index (DEC-70).
		var folded string
		if err := tx.QueryRowContext(ctx, `SELECT LOWER(?)`, name).Scan(&folded); err != nil {
			return nil, err
		}
		claim := fmt.Sprintf("reference-name:%v:%s", ws, folded)
		holder, err := claimKey(ctx, tx, claim)
		if err != nil {
			return nil, err
		}
		if live {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reference_documents WHERE workspace_id=? AND BINARY LOWER(name)=BINARY ? AND deleted_at IS NULL AND id<>?`, ws, folded, id).Scan(&count); err != nil {
				return nil, err
			}
			if holder != "" && holder != id {
				count++
			}
			if err := checkReferenceName(true, count); err != nil {
				return nil, err
			}
			if err := setClaim(ctx, tx, claim, id); err != nil {
				return nil, err
			}
		} else if holder == id {
			if err := setClaim(ctx, tx, claim, ""); err != nil {
				return nil, err
			}
		}
	}
	if w.table == "tasks" {
		if err := taskBranchRule(ctx, tx, w); err != nil {
			return nil, err
		}
		if err := taskIntakeRule(ctx, tx, w); err != nil {
			return nil, err
		}
	}
	if w.table == "workspaces" {
		if err := workspaceNameRule(ctx, tx, w); err != nil {
			return nil, err
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

// taskBranchClaimKey names the lock row that serializes and claims one open
// task branch. AttachTaskBranch, insertTaskRow and writeRow address the same
// row through it (DEC-70, component-persistence Lock and transaction contract).
func taskBranchClaimKey(ws, repo, branch string) string {
	return "task-open-branch:" + ws + ":" + repo + ":" + branch
}

// taskIntakeClaimKey names the lock row that serializes and claims one
// non-empty task intake key. The task insert paths and writeRow address the
// same row (DEC-70, component-persistence Lock and transaction contract).
func taskIntakeClaimKey(ws, key string) string {
	return "task-intake:" + ws + ":" + key
}

// taskIntakeRule keeps non-empty intake keys unique across committed task
// rows and the lock-row claim. Task intake keys are immutable and task rows
// cannot be hard-deleted, so claims are never released (DEC-70,
// component-persistence Lock and transaction contract).
func taskIntakeRule(ctx context.Context, tx *sql.Tx, w rowWrite) error {
	if w.operation != "INSERT" {
		return nil
	}
	id, _ := w.values["id"].(string)
	ws, _ := w.values["workspace_id"].(string)
	if id == "" || ws == "" {
		return fmt.Errorf("task insert requires id and workspace_id")
	}
	raw, present := w.values["intake_key"]
	if !present || raw == nil {
		return nil
	}
	key, ok := raw.(string)
	if !ok {
		return fmt.Errorf("task intake_key must be a string")
	}
	if key == "" {
		return nil
	}
	claim := taskIntakeClaimKey(ws, key)
	holder, err := claimKey(ctx, tx, claim)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE workspace_id=? AND BINARY intake_key=BINARY ? AND id<>?`, ws, key, id).Scan(&count); err != nil {
		return err
	}
	if (holder != "" && holder != id) || count > 0 {
		return fmt.Errorf("task intake key already exists")
	}
	return setClaim(ctx, tx, claim, id)
}

// workspaceNameRule claims the SQL-folded workspace name and checks rows
// written before claims existed. The workspace-registry lock is acquired by
// both public callers before writeRow (DEC-75(4), DEC-70).
func workspaceNameRule(ctx context.Context, tx *sql.Tx, w rowWrite) error {
	if w.operation != "INSERT" {
		return nil
	}
	id, okID := w.values["id"].(string)
	name, okName := w.values["name"].(string)
	if !okID || id == "" || !okName || strings.TrimSpace(name) == "" {
		return fmt.Errorf("workspace insert requires id and name")
	}
	var folded string
	if err := tx.QueryRowContext(ctx, `SELECT LOWER(TRIM(?))`, name).Scan(&folded); err != nil {
		return err
	}
	claim := "workspace-name:" + folded
	holder, err := claimKey(ctx, tx, claim)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE LOWER(TRIM(name))=? AND id<>?`, folded, id).Scan(&count); err != nil {
		return err
	}
	if (holder != "" && holder != id) || count > 0 {
		return store.ErrWorkspaceConflict
	}
	return setClaim(ctx, tx, claim, id)
}

// taskBranchRule keeps one open task per workspace, repo and branch
// (req-task-branch-assignment AC-4.1, AC-4.2). SingleStore has no partial
// unique index, and the lock row and task row live on different partitions,
// so the occupant is claimed on the lock row and released only by the write
// that ends its occupancy: a terminal state or a branch change (DEC-70,
// component-persistence Lock and transaction contract).
func taskBranchRule(ctx context.Context, tx *sql.Tx, w rowWrite) error {
	switch w.operation {
	case "INSERT":
		id, _ := w.values["id"].(string)
		ws, _ := w.values["workspace_id"].(string)
		repo, okRepo := w.values["repo_name"].(string)
		branch, okBranch := w.values["branch"].(string)
		state, okState := w.values["state"]
		if id == "" || ws == "" || !okRepo || !okBranch || !okState {
			return fmt.Errorf("task insert requires id, workspace_id, repo_name, branch and state")
		}
		if core.TaskTerminal(core.TaskState(fmt.Sprint(state))) {
			return nil
		}
		return claimTaskBranch(ctx, tx, ws, repo, branch, id)
	case "UPDATE":
		state, setsState := w.values["state"]
		branch, setsBranch := w.values["branch"]
		repo, setsRepo := w.values["repo_name"]
		if !setsState && !setsBranch && !setsRepo {
			return nil
		}
		ws, _ := w.where["workspace_id"].(string)
		id, _ := w.where["id"].(string)
		if ws == "" || id == "" {
			return fmt.Errorf("task state or branch write requires one task id")
		}
		if _, ok := branch.(string); setsBranch && !ok {
			return fmt.Errorf("task branch must be a string")
		}
		if _, ok := repo.(string); setsRepo && !ok {
			return fmt.Errorf("task repo_name must be a string")
		}
		var oldRepo, oldBranch, oldState sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT repo_name, branch, state FROM tasks WHERE workspace_id=? AND id=?`, ws, id).Scan(&oldRepo, &oldBranch, &oldState)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		newRepo, newBranch, newState := oldRepo.String, oldBranch.String, oldState.String
		if setsRepo {
			newRepo = repo.(string)
		}
		if setsBranch {
			newBranch = branch.(string)
		}
		if setsState {
			newState = fmt.Sprint(state)
		}
		wasOpen := !core.TaskTerminal(core.TaskState(oldState.String))
		isOpen := !core.TaskTerminal(core.TaskState(newState))
		moved := oldRepo.String != newRepo || oldBranch.String != newBranch
		if wasOpen && (!isOpen || moved) {
			if err := releaseTaskBranch(ctx, tx, taskBranchClaimKey(ws, oldRepo.String, oldBranch.String), id); err != nil {
				return err
			}
		}
		if isOpen && (!wasOpen || moved) {
			return claimTaskBranch(ctx, tx, ws, newRepo, newBranch, id)
		}
	}
	return nil
}

// claimTaskBranch refuses a branch whose claim or committed open row belongs
// to another task, then records id as the holder. The claim key and the
// occupant check compare the same exact repo and branch bytes; BINARY drops
// PAD SPACE, as the Postgres tasks_open_repo_branch_idx compares text (DEC-70).
func claimTaskBranch(ctx context.Context, tx *sql.Tx, ws, repo, branch, id string) error {
	key := taskBranchClaimKey(ws, repo, branch)
	holder, err := claimKey(ctx, tx, key)
	if err != nil {
		return err
	}
	if holder != "" && holder != id {
		return store.TaskBranchInUseError(branch, holder)
	}
	var occupant string
	err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE workspace_id=? AND BINARY repo_name=BINARY ? AND BINARY branch=BINARY ? AND state NOT IN ('merged','closed') AND id<>? LIMIT 1`, ws, repo, branch, id).Scan(&occupant)
	if err == nil {
		return store.TaskBranchInUseError(branch, occupant)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return setClaim(ctx, tx, key, id)
}

// releaseTaskBranch clears key only when id holds it, so a stale read of the
// task's own row can never free another task's claim.
func releaseTaskBranch(ctx context.Context, tx *sql.Tx, key, id string) error {
	holder, err := claimKey(ctx, tx, key)
	if err != nil || holder != id {
		return err
	}
	return setClaim(ctx, tx, key, "")
}
