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

// Classification is recorded before any migration DDL. A fresh bootstrap that
// stops after an implicit commit must remain fresh on its next start; an
// existing installation must never acquire these indexes during an upgrade.
func (s *Store) hotReadBootstrapOrigin(ctx context.Context, emptyLedger bool) (bool, error) {
	if _, err := backendExec(ctx, s.db, `CREATE ROWSTORE TABLE IF NOT EXISTS conveyor_singlestore_bootstrap (id INT NOT NULL,origin VARCHAR(16) NOT NULL,PRIMARY KEY(id),SHARD KEY(id))`); err != nil {
		return false, err
	}
	origin := "legacy"
	if emptyLedger {
		origin = "fresh"
	}
	if _, err := backendExec(ctx, s.db, `INSERT IGNORE INTO conveyor_singlestore_bootstrap(id,origin) VALUES (1,?)`, origin); err != nil {
		return false, err
	}
	if err := backendRow(ctx, s.db, `SELECT origin FROM conveyor_singlestore_bootstrap WHERE id=1`).Scan(&origin); err != nil {
		return false, err
	}
	switch origin {
	case "fresh":
		return true, nil
	case "legacy":
		return false, nil
	default:
		return false, fmt.Errorf("invalid SingleStore bootstrap origin %q", origin)
	}
}

func (s *Store) migrateHotReadIndexes(ctx context.Context) error {
	for _, index := range hotReadIndexes {
		if index.column != "" {
			var exists int
			if err := backendRow(ctx, s.db, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, index.table, index.column).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				if _, err := backendExec(ctx, s.db, "ALTER TABLE "+index.table+" ADD COLUMN "+index.column+" AS ("+index.expression+") PERSISTED VARCHAR(64)"); err != nil {
					return err
				}
			}
			if err := s.checkHotReadColumn(ctx, index); err != nil {
				return err
			}
		}
		var exists int
		if err := backendRow(ctx, s.db, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?`, index.table, index.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			statement := "ALTER TABLE " + index.table + " ADD KEY " + index.name + " (" + strings.Join(index.columns, ",") + ")"
			if index.hash {
				statement += " USING HASH"
			}
			if _, err := backendExec(ctx, s.db, statement); err != nil {
				return err
			}
		}
		rows, err := backendRows(ctx, s.db, `SELECT column_name,index_type,non_unique FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? ORDER BY seq_in_index`, index.table, index.name)
		if err != nil {
			return err
		}
		var columns []string
		for rows.Next() {
			var column, kind string
			var nonUnique int
			if err := rows.Scan(&column, &kind, &nonUnique); err != nil {
				rows.Close()
				return err
			}
			if !hotReadIndexTypeMatches(index, kind, nonUnique) {
				rows.Close()
				return fmt.Errorf("incompatible hot-read index %s.%s", index.table, index.name)
			}
			columns = append(columns, column)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !slices.Equal(columns, index.columns) {
			return fmt.Errorf("incompatible hot-read index columns %s.%s", index.table, index.name)
		}
	}
	return nil
}

func (s *Store) checkHotReadColumn(ctx context.Context, index hotReadIndex) error {
	var table, definition string
	if err := backendRow(ctx, s.db, "SHOW CREATE TABLE "+index.table).Scan(&table, &definition); err != nil {
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

func hotReadIndexTypeMatches(index hotReadIndex, kind string, nonUnique int) bool {
	if nonUnique != 1 {
		return false
	}
	kind = strings.ToUpper(strings.TrimSpace(kind))
	if index.hash {
		return kind == "HASH" || kind == "COLUMNSTORE HASH"
	}
	return kind == "BTREE" || kind == "SKIPLIST"
}

func normalizeHotReadExpression(expression string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '`' || r == '(' || r == ')' {
			return -1
		}
		return unicode.ToLower(r)
	}, expression)
}
