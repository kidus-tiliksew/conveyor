package postgres

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// repairDismissalArchives runs inside the migration transaction. Only live
// documents with dismissed history qualify, so retries never append twice.
func repairDismissalArchives(ctx context.Context, tx pgx.Tx) (map[string][]string, error) {
	affected := map[string][]string{}
	for _, tier := range []struct{ kind, table, versions, key, flag, at string }{
		{"requirement", "requirements", "requirement_versions", "requirement_id", "retired", "retired_at"},
		{"system_design", "system_designs", "system_design_versions", "document_id", "dismissed", "dismissed_at"},
	} {
		rows, err := tx.Query(ctx, "SELECT d.workspace_id,d.id FROM "+tier.table+" d WHERE COALESCE(d.current_version,0)=0 AND d.archived_at IS NULL AND EXISTS (SELECT 1 FROM "+tier.versions+" v WHERE v.workspace_id=d.workspace_id AND v."+tier.key+"=d.id AND v."+tier.flag+") AND NOT EXISTS (SELECT 1 FROM "+tier.versions+" v WHERE v.workspace_id=d.workspace_id AND v."+tier.key+"=d.id AND NOT v.confirmed AND NOT v."+tier.flag+") ORDER BY d.workspace_id,d.id")
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
			scoped := store.WithActor(store.WithWorkspace(ctx, c.workspace), store.SystemActor("system"))
			var note string
			if err := tx.QueryRow(scoped, "SELECT COALESCE(dismissal_note,'') FROM "+tier.versions+" WHERE workspace_id=$1 AND "+tier.key+"=$2 AND "+tier.flag+" ORDER BY "+tier.at+" DESC,version DESC LIMIT 1", c.workspace, c.id).Scan(&note); err != nil {
				return nil, err
			}
			scoped, err = store.WithDocumentDismissalNote(scoped, note)
			if err != nil {
				return nil, err
			}
			changed, err := archiveDismissedDocumentTx(scoped, tx, db.New(tx), tier.kind, c.id, time.Now().UTC())
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
