package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) CreateSystemDesign(ctx context.Context, document core.SystemDesign, first core.SystemDesignVersion) (core.SystemDesign, core.SystemDesignVersion, error) {
	document.ID, document.Title, document.Category = strings.TrimSpace(document.ID), strings.TrimSpace(document.Title), strings.TrimSpace(document.Category)
	if document.ID == "" || document.Title == "" || document.Category == "" {
		return document, first, fmt.Errorf("system design id, title, and category are required")
	}
	if err := core.ValidateSystemDesignID(document.ID); err != nil {
		return document, first, err
	}
	if document.Slug == "" {
		document.Slug = core.RequirementSlug(document.Title)
	}
	if err := core.NormalizeSystemDesignVersion(&first); err != nil {
		return document, first, err
	}
	now := time.Now().UTC()
	document.Workspace, document.CurrentVersion = documentWorkspace(ctx), 0
	document.CreatedAt, document.UpdatedAt = now, now
	first.Workspace, first.DocumentID, first.Version = documentWorkspace(ctx), document.ID, 1
	first.Confirmed, first.ConfirmedBy, first.ConfirmedAt, first.CreatedAt = false, "", time.Time{}, now
	first.DismissalNote = ""
	first.Dismissed, first.DismissedBy, first.DismissedAt = false, "", time.Time{}
	governs, _ := json.Marshal(first.Governs)
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var idCount, slugCount int
		if err := documentRow(ctx, tx, `SELECT COUNT(*) FROM system_designs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), document.ID).Scan(&idCount); err != nil {
			return err
		}
		if idCount > 0 {
			return store.ErrSystemDesignIDConflict
		}
		if err := documentRow(ctx, tx, `SELECT COUNT(*) FROM system_designs WHERE workspace_id=? AND slug=?`, documentWorkspace(ctx), document.Slug).Scan(&slugCount); err != nil {
			return err
		}
		if slugCount > 0 {
			return store.ErrSystemDesignSlugConflict
		}

		if _, err := documentExec(ctx, tx, `INSERT INTO system_designs (workspace_id,id,slug,title,category,current_version,created_at,updated_at) VALUES (?,?,?,?,?,NULL,?,?)`, documentWorkspace(ctx), document.ID, document.Slug, document.Title, document.Category, now, now); err != nil {
			return err
		}
		if _, err := documentExec(ctx, tx, `INSERT INTO system_design_versions (workspace_id,document_id,version,content,governs,origin,origin_session_id,origin_task_id,created_at) VALUES (?,?,1,?,?,?,?,?,?)`, documentWorkspace(ctx), document.ID, first.Content, governs, string(first.Origin), nullString(first.OriginSessionID), nullString(first.OriginTaskID), now); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.created", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": document.ID, "title": document.Title, "category": document.Category})}); err != nil {
			return err
		}
		return insertSystemDesignProposalEvent(ctx, tx, first)
	})
	if err != nil {
		var pgErr *mysql.MySQLError
		if errors.As(err, &pgErr) && pgErr.Number == 1062 {
			switch {
			case strings.Contains(pgErr.Message, "PRIMARY"):
				return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", store.ErrSystemDesignIDConflict, document.ID)
			case strings.Contains(pgErr.Message, "system_designs_workspace_id_slug_key"):
				return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", store.ErrSystemDesignSlugConflict, document.Slug)
			}
		}
	}
	return document, first, err
}

func (s *Store) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	item := core.SystemDesign{Workspace: documentWorkspace(ctx), ID: id}
	var current *int
	var archivedAt *time.Time
	err := documentRow(ctx, s.db, `SELECT slug,title,category,current_version,archived_at,archived_by,superseded_by,created_at,updated_at FROM system_designs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id).Scan(&item.Slug, &item.Title, &item.Category, &current, &archivedAt, &item.ArchivedBy, &item.SupersededBy, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: system design %s", store.ErrNotFound, id)
	}
	if current != nil {
		item.CurrentVersion = *current
	}
	if archivedAt != nil {
		item.Archived, item.ArchivedAt = true, *archivedAt
	}
	return item, err
}

func (s *Store) ListSystemDesigns(ctx context.Context, includeArchived bool) ([]core.SystemDesign, error) {
	rows, err := documentRows(ctx, s.db, systemDesignSelect+` WHERE workspace_id=? AND (? OR archived_at IS NULL) ORDER BY category,title,id`, documentWorkspace(ctx), includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.SystemDesign{}
	for rows.Next() {
		item, scanErr := scanSystemDesign(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ArchiveSystemDesign(ctx context.Context, id, actor string, supersededBy []string) error {
	return s.setSystemDesignArchived(ctx, id, actor, true, supersededBy)
}
func (s *Store) RestoreSystemDesign(ctx context.Context, id, actor string) error {
	return s.setSystemDesignArchived(ctx, id, actor, false, nil)
}
func (s *Store) setSystemDesignArchived(ctx context.Context, id, actor string, archived bool, supersededBy []string) error {
	return s.documentTx(ctx, func(tx *sql.Tx) error {
		var current *int
		var archivedAt *time.Time
		var storedSupersedingIDs []string
		if err := documentRow(ctx, tx, `SELECT current_version,archived_at,superseded_by FROM system_designs WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), id).Scan(&current, &archivedAt, &storedSupersedingIDs); err != nil {
			return notFound(err, "system design %s", id)
		}
		if (archivedAt != nil) == archived {
			return nil
		}
		accepted := append([]string{}, storedSupersedingIDs...)
		if archived {
			var err error
			accepted, err = validateSupersededByTx(ctx, tx, documentWorkspace(ctx), id, supersededBy)
			if err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if archived {
			if _, err := documentExec(ctx, tx, `UPDATE system_designs SET archived_at=?,archived_by=?,superseded_by=?,updated_at=? WHERE workspace_id=? AND id=?`, now, actor, accepted, now, documentWorkspace(ctx), id); err != nil {
				return err
			}
		} else if _, err := documentExec(ctx, tx, `UPDATE system_designs SET archived_at=NULL,archived_by='',superseded_by='[]',updated_at=? WHERE workspace_id=? AND id=?`, now, documentWorkspace(ctx), id); err != nil {
			return err
		}
		version := 0
		if current != nil {
			version = *current
		}
		kind := "system_design.restored"
		if archived {
			kind = "system_design.archived"
		}
		payload := map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": id, "version": version, "actor": actor, "at": now, "superseded_by": accepted}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind, Payload: core.JSONPayload(payload)})
	})
}

func (s *Store) ProposeSystemDesignVersion(ctx context.Context, version core.SystemDesignVersion) (core.SystemDesignVersion, error) {
	if err := core.NormalizeSystemDesignVersion(&version); err != nil {
		return version, err
	}
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var latest int
		if err := documentRow(ctx, tx, `SELECT 1 FROM system_designs WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), version.DocumentID).Scan(new(int)); err != nil {
			return notFound(err, "system design %s", version.DocumentID)
		}
		if archived, archiveErr := documentArchivedTx(ctx, tx, "system_designs", documentWorkspace(ctx), version.DocumentID); archiveErr != nil {
			return archiveErr
		} else if archived {
			return &store.SystemDesignArchivedError{DocumentID: version.DocumentID}
		}
		if version.Origin == core.SystemDesignOriginImplementation {
			rows, queryErr := documentRows(ctx, tx, systemDesignVersionSelect+` WHERE workspace_id=? AND document_id=? AND origin=? AND origin_task_id=? AND NOT confirmed AND NOT dismissed ORDER BY version`, documentWorkspace(ctx), version.DocumentID, string(version.Origin), version.OriginTaskID)
			if queryErr != nil {
				return queryErr
			}
			for rows.Next() {
				existing, scanErr := scanSystemDesignVersion(rows, version.DocumentID, 0)
				if scanErr != nil {
					rows.Close()
					return scanErr
				}
				if core.NormalizeSystemDesignContent(existing.Content) == version.Content {
					rows.Close()
					existing.Deduplicated = true
					version = existing
					return nil
				}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				rows.Close()
				return rowsErr
			}
			rows.Close()
		}
		if err := documentRow(ctx, tx, `SELECT coalesce(max(version),0) FROM system_design_versions WHERE workspace_id=? AND document_id=?`, documentWorkspace(ctx), version.DocumentID).Scan(&latest); err != nil {
			return err
		}
		version.Workspace, version.Version, version.Confirmed = documentWorkspace(ctx), latest+1, false
		version.ConfirmedBy, version.ConfirmedAt, version.CreatedAt = "", time.Time{}, time.Now().UTC()
		version.DismissalNote = ""
		version.Dismissed, version.DismissedBy, version.DismissedAt = false, "", time.Time{}
		governs, _ := json.Marshal(version.Governs)
		if _, err := documentExec(ctx, tx, `INSERT INTO system_design_versions (workspace_id,document_id,version,content,governs,origin,origin_session_id,origin_task_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, documentWorkspace(ctx), version.DocumentID, version.Version, version.Content, governs, string(version.Origin), nullString(version.OriginSessionID), nullString(version.OriginTaskID), version.CreatedAt); err != nil {
			return err
		}
		if _, err := documentExec(ctx, tx, `UPDATE system_designs SET updated_at=? WHERE workspace_id=? AND id=?`, version.CreatedAt, documentWorkspace(ctx), version.DocumentID); err != nil {
			return err
		}
		return insertSystemDesignProposalEvent(ctx, tx, version)
	})
	return version, err
}

func insertSystemDesignProposalEvent(ctx context.Context, tx *sql.Tx, version core.SystemDesignVersion) error {
	return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.version_proposed", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": version.DocumentID, "version": version.Version, "origin": version.Origin, "origin_session_id": version.OriginSessionID, "origin_task_id": version.OriginTaskID, "governs": version.Governs})})
}

func (s *Store) ConfirmSystemDesignVersion(ctx context.Context, documentID string, version int, expectedCurrentVersion ...int) (core.SystemDesign, core.SystemDesignVersion, error) {
	var document core.SystemDesign
	var confirmed core.SystemDesignVersion
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var current *int
		if err := documentRow(ctx, tx, `SELECT current_version FROM system_designs WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), documentID).Scan(&current); err != nil {
			return notFound(err, "system design %s", documentID)
		}
		if archived, archiveErr := documentArchivedTx(ctx, tx, "system_designs", documentWorkspace(ctx), documentID); archiveErr != nil {
			return archiveErr
		} else if archived {
			return &store.SystemDesignArchivedError{DocumentID: documentID}
		}
		currentVersion := 0
		if current != nil {
			currentVersion = *current
		}
		if len(expectedCurrentVersion) > 1 {
			return fmt.Errorf("at most one expected current system design version may be supplied")
		}
		if len(expectedCurrentVersion) == 1 && expectedCurrentVersion[0] != currentVersion {
			expected := expectedCurrentVersion[0]
			return &store.SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: currentVersion, Expected: &expected}
		}
		var err error
		confirmed, err = scanSystemDesignVersion(documentRow(ctx, tx, systemDesignVersionSelect+` WHERE workspace_id=? AND document_id=? AND version=?`, documentWorkspace(ctx), documentID, version), documentID, version)
		if err != nil {
			return err
		}
		if confirmed.Confirmed && currentVersion == version {
			document, err = scanSystemDesign(documentRow(ctx, tx, systemDesignSelect+` WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), documentID), documentID)
			return err
		}
		if confirmed.Dismissed {
			return &store.SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: currentVersion}
		}
		if version < currentVersion {
			return &store.SystemDesignVersionConflict{DocumentID: documentID, Requested: version, Current: currentVersion}
		}
		if err = core.NormalizeSystemDesignVersion(&confirmed); err != nil {
			return err
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		dismissedRows, dismissErr := documentRows(ctx, tx, `SELECT version FROM system_design_versions WHERE workspace_id=? AND document_id=? AND version<? AND confirmed=false AND dismissed=false FOR UPDATE`, documentWorkspace(ctx), documentID, version)
		if dismissErr != nil {
			return dismissErr
		}
		var dismissed []int
		for dismissedRows.Next() {
			var dismissedVersion int
			if err = dismissedRows.Scan(&dismissedVersion); err != nil {
				dismissedRows.Close()
				return err
			}
			dismissed = append(dismissed, dismissedVersion)
		}
		if err = dismissedRows.Err(); err != nil {
			dismissedRows.Close()
			return err
		}
		dismissedRows.Close()
		if _, err = documentExec(ctx, tx, `UPDATE system_design_versions SET dismissed=true,dismissed_by=?,dismissed_at=?,dismissal_note=NULLIF(?,'') WHERE workspace_id=? AND document_id=? AND version<? AND confirmed=false AND dismissed=false`, actor.ID, now, store.DocumentDismissalNote(ctx), documentWorkspace(ctx), documentID, version); err != nil {
			return err
		}
		if _, err = documentExec(ctx, tx, `UPDATE system_design_versions SET confirmed=true,confirmed_by=?,confirmed_at=? WHERE workspace_id=? AND document_id=? AND version=?`, actor.ID, now, documentWorkspace(ctx), documentID, version); err != nil {
			return err
		}
		if _, err = documentExec(ctx, tx, `UPDATE system_designs SET current_version=?,updated_at=? WHERE workspace_id=? AND id=?`, version, now, documentWorkspace(ctx), documentID); err != nil {
			return err
		}
		confirmed.Confirmed, confirmed.ConfirmedBy, confirmed.ConfirmedAt = true, actor.ID, now
		document, err = scanSystemDesign(documentRow(ctx, tx, systemDesignSelect+` WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), documentID), documentID)
		if err != nil {
			return err
		}
		for _, dismissedVersion := range dismissed {
			if err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.version_dismissed", Payload: core.JSONPayload(store.DocumentDismissalEventPayload(ctx, map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": dismissedVersion, "dismissed_by": actor.ID, "confirmed_version": version}))}); err != nil {
				return err
			}
		}
		if err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.version_confirmed", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": version, "supersedes_version": currentVersion, "confirmed_by": actor.ID, "origin": confirmed.Origin, "origin_session_id": confirmed.OriginSessionID, "origin_task_id": confirmed.OriginTaskID, "governs": confirmed.Governs})}); err != nil {
			return err
		}
		if err = recomputeDecisionSweepsForDocumentTx(ctx, tx, core.DecisionSweepTierSystemDesign, documentID, confirmed.Content); err != nil {
			return err
		}
		if err = reconcileConfirmedSystemDesignDriftTx(ctx, tx, documentID, version, confirmed.CreatedAt, now); err != nil {
			return err
		}
		return activatePendingTaskContextTx(ctx, tx, documentWorkspace(ctx), documentID, version, true)
	})
	return document, confirmed, err
}

func (s *Store) DismissSystemDesignVersion(ctx context.Context, documentID string, version int) (core.SystemDesign, core.SystemDesignVersion, error) {
	var (
		document  core.SystemDesign
		dismissed core.SystemDesignVersion
	)
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var err error
		document, dismissed, err = dismissSystemDesignVersionTx(ctx, tx, documentID, version)
		return err
	})
	return document, dismissed, err
}

func dismissSystemDesignVersionTx(ctx context.Context, tx *sql.Tx, documentID string, version int) (core.SystemDesign, core.SystemDesignVersion, error) {
	var document core.SystemDesign
	var dismissed core.SystemDesignVersion
	err := func() error {
		var current *int
		if err := documentRow(ctx, tx, `SELECT current_version FROM system_designs
			WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), documentID).Scan(&current); err != nil {
			return notFound(err, "system design %s", documentID)
		}
		currentVersion := 0
		if current != nil {
			currentVersion = *current
		}
		var err error
		dismissed, err = scanSystemDesignVersion(documentRow(ctx, tx, systemDesignVersionSelect+
			` WHERE workspace_id=? AND document_id=? AND version=?`, documentWorkspace(ctx), documentID, version), documentID, version)
		if err != nil {
			return err
		}
		if dismissed.Confirmed {
			return &store.SystemDesignVersionDismissalConflict{DocumentID: documentID, Requested: version, Current: currentVersion, Reason: store.VersionDismissalConfirmed}
		}
		if version < currentVersion {
			return &store.SystemDesignVersionDismissalConflict{DocumentID: documentID, Requested: version, Current: currentVersion, Reason: store.VersionDismissalSuperseded, SupersededBy: currentVersion}
		}
		if dismissed.Dismissed {
			return &store.SystemDesignVersionDismissalConflict{DocumentID: documentID, Requested: version, Current: currentVersion, Reason: store.VersionDismissalDismissed}
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if _, err = documentExec(ctx, tx, `UPDATE system_design_versions
			SET dismissed=true,dismissed_by=?,dismissed_at=?,dismissal_note=NULLIF(?,'')
			WHERE workspace_id=? AND document_id=? AND version=?`, actor.ID, now, store.DocumentDismissalNote(ctx), documentWorkspace(ctx), documentID, version); err != nil {
			return err
		}
		if _, err = documentExec(ctx, tx, `UPDATE system_designs SET updated_at=?
			WHERE workspace_id=? AND id=?`, now, documentWorkspace(ctx), documentID); err != nil {
			return err
		}
		dismissed.DismissalNote = store.DocumentDismissalNote(ctx)
		dismissed.Dismissed, dismissed.DismissedBy, dismissed.DismissedAt = true, actor.ID, now
		if document, err = scanSystemDesign(documentRow(ctx, tx, systemDesignSelect+
			` WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), documentID), documentID); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.version_dismissed", Payload: core.JSONPayload(store.DocumentDismissalEventPayload(ctx, map[string]any{
			"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": version, "dismissed_by": actor.ID,
		}))})
	}()
	return document, dismissed, err
}

func reconcileConfirmedSystemDesignDriftTx(ctx context.Context, tx *sql.Tx, documentID string, version int, proposedAt, resolvedAt time.Time) error {
	rows, err := documentRows(ctx, tx, `SELECT id,COALESCE(task_id,'') FROM repository_drift WHERE workspace_id=? AND system_design_id=? AND kind='lineaged_merge' AND system_design_version<? AND detected_at<? AND resolved_at IS NULL FOR UPDATE`, documentWorkspace(ctx), documentID, version, proposedAt)
	if err != nil {
		return err
	}
	type reconciledDrift struct{ id, taskID string }
	var reconciled []reconciledDrift
	for rows.Next() {
		var drift reconciledDrift
		if err = rows.Scan(&drift.id, &drift.taskID); err != nil {
			rows.Close()
			return err
		}
		reconciled = append(reconciled, drift)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, drift := range reconciled {
		if _, err = documentExec(ctx, tx, `UPDATE repository_drift SET resolved_at=?,outcome='design_document_updated' WHERE workspace_id=? AND id=?`, resolvedAt, documentWorkspace(ctx), drift.id); err != nil {
			return err
		}
		payload := core.JSONPayload(map[string]any{
			"drift_id": drift.id, "outcome": "design_document_updated", "resolved_at": resolvedAt,
			"document_id": documentID, "confirmed_version": version,
		})
		if err = insertEvent(ctx, tx, core.Event{TaskID: drift.taskID, Kind: "monitor.drift_reconciled", At: resolvedAt, Payload: payload}); err != nil {
			return err
		}
		if err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.drift_resolved", At: resolvedAt, Payload: core.JSONPayload(map[string]any{
			"workspace_id": documentWorkspace(ctx), "document_id": documentID, "drift_id": drift.id,
			"outcome": "design_document_updated", "resolved_at": resolvedAt, "confirmed_version": version,
		})}); err != nil {
			return err
		}
	}
	return nil
}

const systemDesignSelect = `SELECT workspace_id,id,slug,title,category,current_version,archived_at,archived_by,superseded_by,created_at,updated_at FROM system_designs`
const systemDesignVersionSelect = `SELECT workspace_id,document_id,version,content,governs,origin,coalesce(origin_session_id,''),coalesce(origin_task_id,''),confirmed,coalesce(confirmed_by,''),confirmed_at,dismissed,coalesce(dismissed_by,''),dismissed_at,created_at,COALESCE(dismissal_note,'') FROM system_design_versions`

func scanSystemDesign(row documentScanner, id string) (core.SystemDesign, error) {
	var item core.SystemDesign
	var current *int
	var archivedAt *time.Time
	err := row.Scan(&item.Workspace, &item.ID, &item.Slug, &item.Title, &item.Category, &current, &archivedAt, &item.ArchivedBy, &item.SupersededBy, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: system design %s", store.ErrNotFound, id)
	}
	if current != nil {
		item.CurrentVersion = *current
	}
	if archivedAt != nil {
		item.Archived, item.ArchivedAt = true, *archivedAt
	}
	return item, err
}
func scanSystemDesignVersion(row documentScanner, id string, version int) (core.SystemDesignVersion, error) {
	var item core.SystemDesignVersion
	var raw []byte
	var origin string
	var confirmedAt *time.Time
	var dismissedAt *time.Time
	err := row.Scan(&item.Workspace, &item.DocumentID, &item.Version, &item.Content, &raw, &origin, &item.OriginSessionID, &item.OriginTaskID, &item.Confirmed, &item.ConfirmedBy, &confirmedAt, &item.Dismissed, &item.DismissedBy, &dismissedAt, &item.CreatedAt, &item.DismissalNote)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: system design %s has no version %d", store.ErrNotFound, id, version)
	}
	if err != nil {
		return item, err
	}
	item.Origin = core.SystemDesignOrigin(origin)
	if err = json.Unmarshal(raw, &item.Governs); err != nil {
		return item, err
	}
	if confirmedAt != nil {
		item.ConfirmedAt = *confirmedAt
	}
	if dismissedAt != nil {
		item.DismissedAt = *dismissedAt
	}
	return item, nil
}

// documentArchivedTx keeps historical migration tests able to seed an older
// schema with the current store before migrating it forward.
func documentArchivedTx(ctx context.Context, tx *sql.Tx, table, workspaceID, id string) (bool, error) {
	if table != "requirements" && table != "system_designs" {
		return false, fmt.Errorf("unsupported archival table %q", table)
	}
	columnExists, err := archivalColumnExistsTx(ctx, tx, table)
	if err != nil || !columnExists {
		return false, err
	}
	var archivedAt *time.Time
	err = documentRow(ctx, tx, fmt.Sprintf(`SELECT archived_at FROM %s WHERE workspace_id=? AND id=?`, table), workspaceID, id).Scan(&archivedAt)
	return archivedAt != nil, err
}

// validateSupersededByTx locks replacement authority while the
// archive transition commits (req-document-operating-surfaces REQ-5/AC-5.7).
func validateSupersededByTx(ctx context.Context, tx *sql.Tx, workspaceID, targetID string, ids []string) ([]string, error) {
	accepted := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, candidate := range ids {
		candidate = strings.TrimSpace(candidate)
		if candidate == targetID {
			return nil, &store.SupersededByInvalidError{DocumentID: candidate, Reason: "is the document being archived"}
		}
		if candidate == "" {
			return nil, &store.SupersededByInvalidError{DocumentID: "(empty)", Reason: "is unknown in this workspace"}
		}
		if seen[candidate] {
			continue
		}
		seen[candidate] = true

		exists, live, archived := false, false, false
		for _, table := range []string{"requirements", "system_designs"} {
			var current *int
			var archivedAt *time.Time
			err := documentRow(ctx, tx, fmt.Sprintf(`SELECT current_version,archived_at FROM %s WHERE workspace_id=? AND id=? FOR UPDATE`, table), workspaceID, candidate).Scan(&current, &archivedAt)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			exists = true
			archived = archived || archivedAt != nil
			live = live || (archivedAt == nil && current != nil && *current > 0)
		}
		if !live {
			reason := "is unknown in this workspace"
			if archived {
				reason = "is archived"
			} else if exists {
				reason = "has no confirmed version"
			}
			return nil, &store.SupersededByInvalidError{DocumentID: candidate, Reason: reason}
		}
		accepted = append(accepted, candidate)
	}
	return accepted, nil
}

func archivalColumnExistsTx(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	return table == "requirements" || table == "system_designs", nil
}

func (s *Store) GetSystemDesignVersion(ctx context.Context, id string, version int) (core.SystemDesignVersion, error) {
	return scanSystemDesignVersion(documentRow(ctx, s.db, systemDesignVersionSelect+` WHERE workspace_id=? AND document_id=? AND version=?`, documentWorkspace(ctx), id, version), id, version)
}
func (s *Store) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	rows, err := documentRows(ctx, s.db, systemDesignVersionSelect+` WHERE workspace_id=? AND document_id=? ORDER BY version`, documentWorkspace(ctx), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.SystemDesignVersion{}
	for rows.Next() {
		item, scanErr := scanSystemDesignVersion(rows, id, 0)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListGovernanceDesigns(ctx context.Context, repository string) ([]core.GovernanceDesignContext, error) {
	rows, err := documentRows(ctx, s.db, `SELECT d.id,d.title,d.category,v.version,v.content,v.governs
		FROM system_designs d JOIN system_design_versions v
		  ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
		WHERE d.workspace_id=? AND d.archived_at IS NULL ORDER BY d.id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.GovernanceDesignContext, 0)
	for rows.Next() {
		var item core.GovernanceDesignContext
		var governs []byte
		if err = rows.Scan(&item.ID, &item.Title, &item.Category, &item.Version, &item.Content, &governs); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(governs, &item.Governs); err != nil {
			return nil, err
		}
		for _, scope := range item.Governs {
			if scope.Repository == repository {
				out = append(out, item)
				break
			}
		}
	}
	return out, rows.Err()
}

func (s *Store) ListPendingSystemDesignVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error) {
	rows, err := documentRows(ctx, s.db, systemDesignVersionSelect+` WHERE workspace_id=? AND origin=? AND origin_task_id=? AND NOT confirmed AND NOT dismissed ORDER BY document_id,version`, documentWorkspace(ctx), string(core.SystemDesignOriginImplementation), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.SystemDesignVersion, 0)
	for rows.Next() {
		item, scanErr := scanSystemDesignVersion(rows, "", 0)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListSystemDesignProposalVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error) {
	rows, err := documentRows(ctx, s.db, systemDesignVersionSelect+` WHERE workspace_id=? AND origin=? AND origin_task_id=? AND NOT dismissed ORDER BY document_id,version`, documentWorkspace(ctx), string(core.SystemDesignOriginImplementation), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.SystemDesignVersion, 0)
	for rows.Next() {
		item, scanErr := scanSystemDesignVersion(rows, "", 0)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListSystemDesignProposalEventsForTask(ctx context.Context, taskID string) ([]core.Event, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=? AND kind='system_design.version_proposed' AND JSON_EXTRACT_STRING(payload_json,'origin_task_id')=? ORDER BY id`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Event, 0)
	for rows.Next() {
		var row core.Event
		var eventWorkspace string
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.Payload, &row.At, &eventWorkspace); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
func (s *Store) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	rows, err := documentRows(ctx, s.db, systemDesignVersionSelect+` WHERE workspace_id=? ORDER BY document_id,version`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.SystemDesignVersion{}
	for rows.Next() {
		item, scanErr := scanSystemDesignVersion(rows, "", 0)
		if scanErr != nil {
			return nil, scanErr
		}
		out[item.DocumentID] = append(out[item.DocumentID], item)
	}
	return out, rows.Err()
}
func (s *Store) ListSystemDesignEvents(ctx context.Context, id string) ([]core.Event, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=? AND kind LIKE 'system_design.%' AND JSON_EXTRACT_STRING(payload_json,'document_id')=? ORDER BY id`, documentWorkspace(ctx), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var row core.Event
		var eventWorkspace string
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.Payload, &row.At, &eventWorkspace); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=? AND kind LIKE 'system_design.%' ORDER BY id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.Event{}
	for rows.Next() {
		var row core.Event
		var eventWorkspace string
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.Payload, &row.At, &eventWorkspace); err != nil {
			return nil, err
		}
		var payload map[string]any
		if json.Unmarshal(row.Payload, &payload) != nil {
			continue
		}
		if documentID, _ := payload["document_id"].(string); documentID != "" {
			out[documentID] = append(out[documentID], row)
		}
	}
	return out, rows.Err()
}

func (s *Store) RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error {
	return s.documentTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM system_design_versions WHERE workspace_id=? AND document_id=? AND version=?)
			AND CASE WHEN ? <> '' THEN EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=? AND id=?)
			ELSE EXISTS(SELECT 1 FROM planning_sessions WHERE workspace_id=? AND id=?) END`, documentWorkspace(ctx), documentID, version, workOrderID, documentWorkspace(ctx), workOrderID, documentWorkspace(ctx), sessionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: system design consultation target", store.ErrNotFound)
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.consulted", Payload: core.JSONPayload(map[string]any{
			"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": version,
			"session_id": sessionID, "work_order_id": workOrderID,
		})})
	})
}

func (s *Store) ListActiveSystemDesignDriftCounts(ctx context.Context) (map[string]int, error) {
	rows, err := documentRows(ctx, s.db, `SELECT system_design_id,COUNT(*) FROM repository_drift WHERE workspace_id=? AND resolved_at IS NULL AND system_design_id IS NOT NULL GROUP BY system_design_id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err = rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
