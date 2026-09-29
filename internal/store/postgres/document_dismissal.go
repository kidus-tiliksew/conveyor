package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// archiveDismissedDocumentTx shares the document lock with proposal and
// confirmation writes. Projection and ordered audit events commit together
// (req-document-operating-surfaces REQ-5; component-document-corpus).
func archiveDismissedDocumentTx(ctx context.Context, tx pgx.Tx, q *db.Queries, kind, id string, now time.Time) (bool, error) {
	table, versions, idKey, flag := "requirements", "requirement_versions", "requirement_id", "retired"
	switch kind {
	case "requirement":
	case "system_design":
		table, versions, idKey, flag = "system_designs", "system_design_versions", "document_id", "dismissed"
	default:
		return false, fmt.Errorf("unsupported dismissal archive kind %q", kind)
	}
	var current int
	var archivedAt *time.Time
	if err := tx.QueryRow(ctx, "SELECT COALESCE(current_version,0),archived_at FROM "+table+" WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id).Scan(&current, &archivedAt); err != nil {
		return false, err
	}
	if current != 0 || archivedAt != nil {
		return false, nil
	}
	var pending bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+versions+" WHERE workspace_id=$1 AND "+idKey+"=$2 AND NOT confirmed AND NOT "+flag+")", workspace(ctx), id).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	actor := store.ActorFromContext(ctx)
	if _, err := tx.Exec(ctx, "UPDATE "+table+" SET archived_at=$3,archived_by=$4,archive_reason=$5,archive_note=$6,superseded_by='{}',updated_at=$3 WHERE workspace_id=$1 AND id=$2", workspace(ctx), id, now, actor.ID, core.ArchiveReasonOnlyProposalDismissed, store.DocumentDismissalNote(ctx)); err != nil {
		return false, err
	}
	err := insertWorkspaceEvent(ctx, q, core.Event{Kind: kind + ".archived", Payload: core.JSONPayload(store.DismissalArchiveEventPayload(ctx, workspace(ctx), idKey, id, actor.ID, now))})
	return err == nil, err
}
