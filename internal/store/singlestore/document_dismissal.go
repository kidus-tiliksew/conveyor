package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// archiveDismissedDocumentTx shares the document lock with proposal and
// confirmation writes. Projection and ordered audit events commit together
// (req-document-operating-surfaces REQ-5; component-document-corpus).
func archiveDismissedDocumentTx(ctx context.Context, tx *sql.Tx, kind, id string, now time.Time) (bool, error) {
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
	if err := documentRow(ctx, tx, "SELECT COALESCE(current_version,0),archived_at FROM "+table+" WHERE workspace_id=? AND id=? FOR UPDATE", documentWorkspace(ctx), id).Scan(&current, &archivedAt); err != nil {
		return false, err
	}
	if current != 0 || archivedAt != nil {
		return false, nil
	}
	var pending bool
	if err := documentRow(ctx, tx, "SELECT EXISTS(SELECT 1 FROM "+versions+" WHERE workspace_id=? AND "+idKey+"=? AND NOT confirmed AND NOT "+flag+")", documentWorkspace(ctx), id).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	actor := store.ActorFromContext(ctx)
	if _, err := documentExec(ctx, tx, "UPDATE "+table+" SET archived_at=?,archived_by=?,archive_reason=?,archive_note=?,superseded_by='[]',updated_at=? WHERE workspace_id=? AND id=?", now, actor.ID, core.ArchiveReasonOnlyProposalDismissed, store.DocumentDismissalNote(ctx), now, documentWorkspace(ctx), id); err != nil {
		return false, err
	}
	err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind + ".archived", Payload: core.JSONPayload(store.DismissalArchiveEventPayload(ctx, documentWorkspace(ctx), idKey, id, actor.ID, now))})
	return err == nil, err
}
