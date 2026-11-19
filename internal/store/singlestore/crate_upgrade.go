package singlestore

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// crateMigrationVersion is the migration that adds the crates and
// crate_requirements tables. Its Go step adds tasks.crate_id after the file
// runs (feature-crates CR-1; component-crates).
const crateMigrationVersion = 45

// crateTaskColumn is the nullable membership column. SingleStore DDL commits
// implicitly and has no ADD COLUMN IF NOT EXISTS, so migrate adds it through
// addNullableColumns, which checks information_schema under the startup
// migration lock, and a rerun after an interrupted startup adds nothing.
// There is no backfill: every existing task belongs to no crate.
var crateTaskColumn = nullableColumn{"tasks", "crate_id", "VARCHAR(64) NULL"}

// crateMemberKeyMigrationVersion is the migration whose Go step adds
// crateMemberKey for the bounded work-order crate member read
// (component-crates invariant 20; component-persistence invariant 36).
const crateMemberKeyMigrationVersion = 47

// crateMemberKey names the ordered rowstore key on tasks and its columns in
// key order.
var crateMemberKey = struct {
	name    string
	columns []string
}{"tasks_workspace_crate_created_idx", []string{"workspace_id", "crate_id", "created_at", "id"}}

// migrateCrateMemberKey adds crateMemberKey to tasks. SingleStore has no
// CREATE INDEX IF NOT EXISTS and commits DDL implicitly, so the step reads
// information_schema under the startup migration lock. A key that a start
// added before it was interrupted is accepted only when it is a non-unique
// ordered key on exactly those columns in that order. A key of that name
// with any other definition refuses the start and changes nothing. The step
// changes no task row.
func (s *Store) migrateCrateMemberKey(ctx context.Context) error {
	keys, err := s.hotReadTableKeys(ctx, "tasks")
	if err != nil {
		return err
	}
	if parts, ok := keys[crateMemberKey.name]; ok {
		return checkCrateMemberKey(parts)
	}
	if _, err := backendExec(ctx, s.db, "ALTER TABLE tasks ADD KEY "+crateMemberKey.name+" ("+strings.Join(crateMemberKey.columns, ",")+")"); err != nil {
		return err
	}
	if keys, err = s.hotReadTableKeys(ctx, "tasks"); err != nil {
		return err
	}
	return checkCrateMemberKey(keys[crateMemberKey.name])
}

// checkCrateMemberKey accepts only a non-unique BTREE or SKIPLIST key whose
// columns match crateMemberKey in order. A missing key has no parts and
// fails the column check.
func checkCrateMemberKey(parts []hotReadKeyPart) error {
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		kind := strings.ToUpper(strings.TrimSpace(part.kind))
		if part.nonUnique != 1 || (kind != "BTREE" && kind != "SKIPLIST") {
			return fmt.Errorf("incompatible crate member key tasks.%s: want a non-unique ordered key", crateMemberKey.name)
		}
		columns = append(columns, strings.ToLower(part.column))
	}
	if !slices.Equal(columns, crateMemberKey.columns) {
		return fmt.Errorf("incompatible crate member key tasks.%s: columns %v, want %v", crateMemberKey.name, columns, crateMemberKey.columns)
	}
	return nil
}
