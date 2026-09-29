package singlestore

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// repairDismissalArchives runs inside the migration transaction. Only live
// documents with dismissed history qualify, so retries never append twice.
func repairDismissalArchives(ctx context.Context, tx *sql.Tx) (map[string][]string, error) {
	affected := map[string][]string{}
	for _, tier := range []struct{ kind, table, versions, key, flag, at string }{
		{"requirement", "requirements", "requirement_versions", "requirement_id", "retired", "retired_at"},
		{"system_design", "system_designs", "system_design_versions", "document_id", "dismissed", "dismissed_at"},
	} {
		rows, err := tx.QueryContext(ctx, "SELECT d.workspace_id,d.id FROM "+tier.table+" d WHERE COALESCE(d.current_version,0)=0 AND d.archived_at IS NULL AND EXISTS (SELECT 1 FROM "+tier.versions+" v WHERE v.workspace_id=d.workspace_id AND v."+tier.key+"=d.id AND v."+tier.flag+") AND NOT EXISTS (SELECT 1 FROM "+tier.versions+" v WHERE v.workspace_id=d.workspace_id AND v."+tier.key+"=d.id AND NOT v.confirmed AND NOT v."+tier.flag+") ORDER BY d.workspace_id,d.id")
		if err != nil {
			return nil, err
		}
		type candidate struct{ workspace, id string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err = rows.Scan(&c.workspace, &c.id); err != nil {
				rows.Close()
				return nil, err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			scoped := store.WithActor(store.WithWorkspace(ctx, c.workspace), store.Actor{ID: "system", Role: core.ActorSystem})
			var note string
			if err := documentRow(scoped, tx, "SELECT COALESCE(dismissal_note,'') FROM "+tier.versions+" WHERE workspace_id=? AND "+tier.key+"=? AND "+tier.flag+" ORDER BY "+tier.at+" DESC,version DESC LIMIT 1", c.workspace, c.id).Scan(&note); err != nil {
				return nil, err
			}
			scoped, err = store.WithDocumentDismissalNote(scoped, note)
			if err != nil {
				return nil, err
			}
			changed, err := archiveDismissedDocumentTx(scoped, tx, tier.kind, c.id, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			if changed {
				affected[c.workspace] = append(affected[c.workspace], tier.kind+":"+c.id)
			}
		}
	}
	return affected, nil
}

func logDismissalArchiveRepair(affected map[string][]string) {
	workspaces := make([]string, 0, len(affected))
	for workspace := range affected {
		workspaces = append(workspaces, workspace)
	}
	sort.Strings(workspaces)
	for _, workspace := range workspaces {
		sort.Strings(affected[workspace])
		slog.Info("dismissal archive migration", "workspace", workspace, "documents", affected[workspace])
	}
}

// SingleStore DDL commits implicitly. The startup lock protects each column
// check; projection repair and its events use a separate atomic transaction.
func (s *Store) migrateDocumentDismissalArchival(ctx context.Context) error {
	for _, table := range []string{"requirements", "system_designs"} {
		for _, column := range []string{"archive_reason", "archive_note"} {
			var exists int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, column).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" TEXT NOT NULL DEFAULT ''"); err != nil {
					return err
				}
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	affected, err := repairDismissalArchives(ctx, tx)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	logDismissalArchiveRepair(affected)
	return nil
}
