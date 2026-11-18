package singlestore

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const hotReadIndexesMigrationVersion = 44

type hotReadIndex struct {
	table, column, expression, name string
	columns                         []string
	hash                            bool
}

// The fresh-bootstrap hook and operator preparation script implement this
// manifest (component-persistence, Read migration invariants).
var hotReadIndexes = []hotReadIndex{
	{"events_columnstore", "event_document_key", "SHA2(JSON_EXTRACT_STRING(payload_json,'document_id'),256)", "events_document_key_idx", []string{"workspace_id", "event_document_key"}, true},
	{"events_columnstore", "event_requirement_key", "SHA2(JSON_EXTRACT_STRING(payload_json,'requirement_id'),256)", "events_requirement_key_idx", []string{"workspace_id", "event_requirement_key"}, true},
	{"events_columnstore", "event_session_key", "SHA2(JSON_EXTRACT_STRING(payload_json,'session_id'),256)", "events_session_key_idx", []string{"workspace_id", "event_session_key"}, true},
	{"tasks", "task_state_key", "SHA2(state,256)", "tasks_workspace_state_created_idx", []string{"workspace_id", "task_state_key", "created_at", "id"}, false},
	{"tasks", "task_intake_key", "SHA2(COALESCE(intake_key,''),256)", "tasks_workspace_intake_lookup_idx", []string{"workspace_id", "task_intake_key"}, false},
	{"tasks", "", "", "tasks_workspace_created_idx", []string{"workspace_id", "created_at", "id"}, false},
	{"artifacts", "", "", "artifacts_workspace_created_idx", []string{"workspace_id", "created_at", "id"}, false},
	{"artifact_links", "artifact_lookup_key", "SHA2(artifact_id,256)", "artifact_links_artifact_lookup_idx", []string{"workspace_id", "artifact_lookup_key"}, true},
	{"artifact_links", "task_lookup_key", "SHA2(COALESCE(task_id,''),256)", "artifact_links_task_lookup_idx", []string{"workspace_id", "task_lookup_key"}, true},
	{"artifact_links", "requirement_lookup_key", "SHA2(COALESCE(requirement_id,''),256)", "artifact_links_requirement_lookup_idx", []string{"workspace_id", "requirement_lookup_key"}, true},
	{"artifact_links", "session_lookup_key", "SHA2(COALESCE(planning_session_id,''),256)", "artifact_links_session_lookup_idx", []string{"workspace_id", "session_lookup_key"}, true},
}

// hotReadCoreTables are the Conveyor-created tables 0044 may prepare. The
// ids are their persisted per-table provenance rows; id 1 is the bootstrap.
var hotReadCoreTables = map[string]int{"tasks": 2, "artifacts": 3, "artifact_links": 4}

// Provenance is recorded per table before any migration DDL. A fresh bootstrap
// that stops after an implicit commit keeps its decisions on the next start; an
// existing or operator-created table never acquires these indexes during an
// upgrade. A table is fresh only if the ledger was empty and the table was
// absent when Conveyor first looked. events_columnstore never counts: it is
// always operator-created. The result maps a core table to "0044 may prepare it".
func (s *Store) hotReadBootstrapOrigin(ctx context.Context, emptyLedger bool) (map[string]bool, error) {
	if _, err := backendExec(ctx, s.db, `CREATE ROWSTORE TABLE IF NOT EXISTS conveyor_singlestore_bootstrap (id INT NOT NULL,origin VARCHAR(16) NOT NULL,PRIMARY KEY(id),SHARD KEY(id))`); err != nil {
		return nil, err
	}
	global := "legacy"
	if emptyLedger {
		global = "fresh"
	}
	origins := map[int]string{1: global}
	for table, id := range hotReadCoreTables {
		origin := "legacy"
		if global == "fresh" {
			var existing int
			if err := backendRow(ctx, s.db, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&existing); err != nil {
				return nil, err
			}
			if existing == 0 {
				origin = "fresh"
			}
		}
		origins[id] = origin
	}
	for id, origin := range origins {
		if _, err := backendExec(ctx, s.db, `INSERT IGNORE INTO conveyor_singlestore_bootstrap(id,origin) VALUES (?,?)`, id, origin); err != nil {
			return nil, err
		}
	}
	stored := map[int]string{}
	rows, err := backendRows(ctx, s.db, `SELECT id,origin FROM conveyor_singlestore_bootstrap`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int
		var origin string
		if err := rows.Scan(&id, &origin); err != nil {
			rows.Close()
			return nil, err
		}
		stored[id] = origin
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, origin := range stored {
		if origin != "fresh" && origin != "legacy" {
			return nil, fmt.Errorf("invalid SingleStore bootstrap origin %q", origin)
		}
	}
	fresh := make(map[string]bool, len(hotReadCoreTables))
	for table, id := range hotReadCoreTables {
		// An id-1 legacy record (or a missing per-table row) is legacy.
		fresh[table] = stored[1] == "fresh" && stored[id] == "fresh"
	}
	return fresh, nil
}

func (s *Store) migrateHotReadIndexes(ctx context.Context, fresh map[string]bool) error {
	var tables []string
	byTable := map[string][]hotReadIndex{}
	for _, index := range hotReadIndexes {
		// events_columnstore is operator-owned; its columns and keys come from
		// docs/sql/events-columnstore/01-create.sql, never from startup DDL.
		// A core table that existed before this bootstrap is operator-owned too.
		if index.table == "events_columnstore" || !fresh[index.table] {
			continue
		}
		if _, ok := byTable[index.table]; !ok {
			tables = append(tables, index.table)
		}
		byTable[index.table] = append(byTable[index.table], index)
	}
	for _, table := range tables {
		if err := s.migrateHotReadTable(ctx, table, byTable[table]); err != nil {
			return err
		}
	}
	return nil
}

type hotReadKeyPart struct {
	column, kind string
	nonUnique    int
}

// Missing columns and keys share one ALTER per fresh core table. On resume,
// existing definitions must pass validation before any missing objects are added.
func (s *Store) migrateHotReadTable(ctx context.Context, table string, indexes []hotReadIndex) error {
	columnstore, err := s.hotReadTableColumnstore(ctx, table)
	if err != nil {
		return err
	}
	columns, err := s.hotReadColumnSet(ctx, table)
	if err != nil {
		return err
	}
	keys, err := s.hotReadTableKeys(ctx, table)
	if err != nil {
		return err
	}
	// Existing definitions are validated before any DDL, so a wrong object fails
	// closed before anything else in this table is added.
	var clauses []string
	var present []hotReadIndex
	addedColumns := false
	for _, index := range indexes {
		if index.column == "" {
			continue
		}
		if columns[strings.ToLower(index.column)] {
			present = append(present, index)
			continue
		}
		clauses = append(clauses, "ADD COLUMN "+index.column+" AS ("+index.expression+") PERSISTED VARCHAR(64)")
		columns[strings.ToLower(index.column)] = true
		addedColumns = true
	}
	if len(present) > 0 {
		definition, err := s.hotReadTableDefinition(ctx, table)
		if err != nil {
			return err
		}
		for _, index := range present {
			if err := validateHotReadColumn(index, definition); err != nil {
				return err
			}
		}
	}
	for _, index := range indexes {
		if parts, ok := keys[strings.ToLower(index.name)]; ok {
			if err := validateHotReadKeyParts(index, columnstore, parts); err != nil {
				return err
			}
		}
	}
	addedKeys := false
	for _, index := range indexes {
		if _, ok := keys[strings.ToLower(index.name)]; ok {
			continue
		}
		statement := "ADD KEY " + index.name + " (" + strings.Join(index.columns, ",") + ")"
		if index.hash && columnstore {
			statement += " USING HASH"
		}
		clauses = append(clauses, statement)
		addedKeys = true
	}
	if len(clauses) > 0 {
		if _, err := backendExec(ctx, s.db, "ALTER TABLE "+table+" "+strings.Join(clauses, ", ")); err != nil {
			return err
		}
	}
	if addedColumns {
		definition, err := s.hotReadTableDefinition(ctx, table)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			if index.column == "" {
				continue
			}
			if err := validateHotReadColumn(index, definition); err != nil {
				return err
			}
		}
	}
	if addedKeys {
		if keys, err = s.hotReadTableKeys(ctx, table); err != nil {
			return err
		}
		for _, index := range indexes {
			if err := validateHotReadKeyParts(index, columnstore, keys[strings.ToLower(index.name)]); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateHotReadKeyParts checks one key's engine kind, non-uniqueness, and
// exact column order. A missing key has no parts and fails the column check.
func validateHotReadKeyParts(index hotReadIndex, columnstore bool, parts []hotReadKeyPart) error {
	var columns []string
	for _, part := range parts {
		if !hotReadIndexTypeMatches(index, columnstore, part.kind, part.nonUnique) {
			return fmt.Errorf("incompatible hot-read index %s.%s", index.table, index.name)
		}
		columns = append(columns, part.column)
	}
	if !slices.Equal(columns, index.columns) {
		return fmt.Errorf("incompatible hot-read index columns %s.%s", index.table, index.name)
	}
	return nil
}

// hotReadColumnSet returns the lower-cased column names of one table, matching
// the case-insensitive comparison the per-column lookup used before.
func (s *Store) hotReadColumnSet(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := backendRows(ctx, s.db, `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`, table)
	if err != nil {
		return nil, err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return nil, err
		}
		columns[strings.ToLower(column)] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return columns, nil
}

// hotReadTableKeys returns each lower-cased index name of one table with its
// parts in key order, read in a single statistics query.
func (s *Store) hotReadTableKeys(ctx context.Context, table string) (map[string][]hotReadKeyPart, error) {
	rows, err := backendRows(ctx, s.db, `SELECT index_name,column_name,index_type,non_unique FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? ORDER BY index_name,seq_in_index`, table)
	if err != nil {
		return nil, err
	}
	keys := map[string][]hotReadKeyPart{}
	for rows.Next() {
		var name string
		var part hotReadKeyPart
		if err := rows.Scan(&name, &part.column, &part.kind, &part.nonUnique); err != nil {
			rows.Close()
			return nil, err
		}
		name = strings.ToLower(name)
		keys[name] = append(keys[name], part)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Store) hotReadTableDefinition(ctx context.Context, table string) (string, error) {
	var name, definition string
	if err := backendRow(ctx, s.db, "SHOW CREATE TABLE "+table).Scan(&name, &definition); err != nil {
		return "", err
	}
	return definition, nil
}

func (s *Store) checkHotReadColumn(ctx context.Context, index hotReadIndex) error {
	definition, err := s.hotReadTableDefinition(ctx, index.table)
	if err != nil {
		return err
	}
	return validateHotReadColumn(index, definition)
}

func validateHotReadColumn(index hotReadIndex, definition string) error {
	pattern := regexp.MustCompile("(?im)^\\s*`?" + regexp.QuoteMeta(index.column) + "`?\\s+AS\\s+(.*?)\\s+PERSISTED\\s+(VARCHAR\\(64\\))(?:\\s|,|$)")
	match := pattern.FindStringSubmatch(definition)
	if len(match) == 0 || normalizeHotReadExpression(match[1]) != normalizeHotReadExpression(index.expression) {
		return fmt.Errorf("incompatible hot-read computed column %s.%s", index.table, index.column)
	}
	return nil
}

// hotReadIndexTypeMatches accepts the key kind the storage engine can hold:
// hash-target keys are HASH on columnstore and plain (BTREE/SKIPLIST) on
// rowstore, where SingleStore rejects non-unique hash keys.
func hotReadIndexTypeMatches(index hotReadIndex, columnstore bool, kind string, nonUnique int) bool {
	if nonUnique != 1 {
		return false
	}
	kind = strings.ToUpper(strings.TrimSpace(kind))
	if index.hash && columnstore {
		return kind == "HASH" || kind == "COLUMNSTORE HASH"
	}
	return kind == "BTREE" || kind == "SKIPLIST"
}

func (s *Store) hotReadTableColumnstore(ctx context.Context, table string) (bool, error) {
	var storage string
	if err := backendRow(ctx, s.db, `SELECT storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&storage); err != nil {
		return false, err
	}
	return strings.EqualFold(storage, "COLUMNSTORE"), nil
}

func normalizeHotReadExpression(expression string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '`' || r == '(' || r == ')' {
			return -1
		}
		return unicode.ToLower(r)
	}, expression)
}
