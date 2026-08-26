package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
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
	document.Workspace, document.CurrentVersion = workspace(ctx), 0
	document.CreatedAt, document.UpdatedAt = now, now
	first.Workspace, first.DocumentID, first.Version = workspace(ctx), document.ID, 1
	first.Confirmed, first.ConfirmedBy, first.ConfirmedAt, first.CreatedAt = false, "", time.Time{}, now
	first.Dismissed, first.DismissedBy, first.DismissedAt = false, "", time.Time{}
	governs, _ := json.Marshal(first.Governs)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO system_designs (workspace_id,id,slug,title,category,current_version,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,NULL,$6,$6)`, workspace(ctx), document.ID, document.Slug, document.Title, document.Category, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO system_design_versions (workspace_id,document_id,version,content,governs,origin,origin_session_id,origin_task_id,created_at) VALUES ($1,$2,1,$3,$4,$5,$6,$7,$8)`, workspace(ctx), document.ID, first.Content, governs, string(first.Origin), nullString(first.OriginSessionID), nullString(first.OriginTaskID), now); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.created", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": document.ID, "title": document.Title, "category": document.Category})}); err != nil {
			return err
		}
		return insertSystemDesignProposalEvent(ctx, q, first)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "system_designs_pkey":
				return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", store.ErrSystemDesignIDConflict, document.ID)
			case "system_designs_workspace_id_slug_key":
				return core.SystemDesign{}, core.SystemDesignVersion{}, fmt.Errorf("%w: %s", store.ErrSystemDesignSlugConflict, document.Slug)
			}
		}
	}
	return document, first, err
}

func (s *Store) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	item := core.SystemDesign{Workspace: workspace(ctx), ID: id}
	var current *int
	err := s.pool.QueryRow(ctx, `SELECT slug,title,category,current_version,created_at,updated_at FROM system_designs WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id).Scan(&item.Slug, &item.Title, &item.Category, &current, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, fmt.Errorf("%w: system design %s", store.ErrNotFound, id)
	}
	if current != nil {
		item.CurrentVersion = *current
	}
	return item, err
}

func (s *Store) ListSystemDesigns(ctx context.Context) ([]core.SystemDesign, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,slug,title,category,current_version,created_at,updated_at FROM system_designs WHERE workspace_id=$1 ORDER BY category,title,id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.SystemDesign{}
	for rows.Next() {
		item := core.SystemDesign{Workspace: workspace(ctx)}
		var current *int
		if err = rows.Scan(&item.ID, &item.Slug, &item.Title, &item.Category, &current, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if current != nil {
			item.CurrentVersion = *current
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ProposeSystemDesignVersion(ctx context.Context, version core.SystemDesignVersion) (core.SystemDesignVersion, error) {
	if err := core.NormalizeSystemDesignVersion(&version); err != nil {
		return version, err
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var latest int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM system_designs WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), version.DocumentID).Scan(new(int)); err != nil {
			return notFound(err, "system design %s", version.DocumentID)
		}
		if version.Origin == core.SystemDesignOriginImplementation {
			rows, queryErr := tx.Query(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND document_id=$2 AND origin=$3 AND origin_task_id=$4 AND NOT confirmed AND NOT dismissed ORDER BY version`, workspace(ctx), version.DocumentID, string(version.Origin), version.OriginTaskID)
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
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version),0) FROM system_design_versions WHERE workspace_id=$1 AND document_id=$2`, workspace(ctx), version.DocumentID).Scan(&latest); err != nil {
			return err
		}
		version.Workspace, version.Version, version.Confirmed = workspace(ctx), latest+1, false
		version.ConfirmedBy, version.ConfirmedAt, version.CreatedAt = "", time.Time{}, time.Now().UTC()
		version.Dismissed, version.DismissedBy, version.DismissedAt = false, "", time.Time{}
		governs, _ := json.Marshal(version.Governs)
		if _, err := tx.Exec(ctx, `INSERT INTO system_design_versions (workspace_id,document_id,version,content,governs,origin,origin_session_id,origin_task_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, workspace(ctx), version.DocumentID, version.Version, version.Content, governs, string(version.Origin), nullString(version.OriginSessionID), nullString(version.OriginTaskID), version.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE system_designs SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), version.DocumentID, version.CreatedAt); err != nil {
			return err
		}
		return insertSystemDesignProposalEvent(ctx, q, version)
	})
	return version, err
}

func insertSystemDesignProposalEvent(ctx context.Context, q *db.Queries, version core.SystemDesignVersion) error {
	return insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.version_proposed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": version.DocumentID, "version": version.Version, "origin": version.Origin, "origin_session_id": version.OriginSessionID, "origin_task_id": version.OriginTaskID, "governs": version.Governs})})
}

func (s *Store) ConfirmSystemDesignVersion(ctx context.Context, documentID string, version int, expectedCurrentVersion ...int) (core.SystemDesign, core.SystemDesignVersion, error) {
	var document core.SystemDesign
	var confirmed core.SystemDesignVersion
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current *int
		if err := tx.QueryRow(ctx, `SELECT current_version FROM system_designs WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), documentID).Scan(&current); err != nil {
			return notFound(err, "system design %s", documentID)
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
		confirmed, err = scanSystemDesignVersion(tx.QueryRow(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND document_id=$2 AND version=$3`, workspace(ctx), documentID, version), documentID, version)
		if err != nil {
			return err
		}
		if confirmed.Confirmed && currentVersion == version {
			document, err = scanSystemDesign(tx.QueryRow(ctx, systemDesignSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID), documentID)
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
		dismissedRows, dismissErr := tx.Query(ctx, `UPDATE system_design_versions SET dismissed=true,dismissed_by=$4,dismissed_at=$5 WHERE workspace_id=$1 AND document_id=$2 AND version<$3 AND confirmed=false AND dismissed=false RETURNING version`, workspace(ctx), documentID, version, actor.ID, now)
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
		if _, err = tx.Exec(ctx, `UPDATE system_design_versions SET confirmed=true,confirmed_by=$4,confirmed_at=$5 WHERE workspace_id=$1 AND document_id=$2 AND version=$3`, workspace(ctx), documentID, version, actor.ID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE system_designs SET current_version=$3,updated_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID, version, now); err != nil {
			return err
		}
		confirmed.Confirmed, confirmed.ConfirmedBy, confirmed.ConfirmedAt = true, actor.ID, now
		document, err = scanSystemDesign(tx.QueryRow(ctx, systemDesignSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), documentID), documentID)
		if err != nil {
			return err
		}
		for _, dismissedVersion := range dismissed {
			if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.version_dismissed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": documentID, "version": dismissedVersion, "dismissed_by": actor.ID, "confirmed_version": version})}); err != nil {
				return err
			}
		}
		if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.version_confirmed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "document_id": documentID, "version": version, "supersedes_version": currentVersion, "confirmed_by": actor.ID, "origin": confirmed.Origin, "origin_session_id": confirmed.OriginSessionID, "origin_task_id": confirmed.OriginTaskID, "governs": confirmed.Governs})}); err != nil {
			return err
		}
		if err = recomputeDecisionSweepsForDocumentTx(ctx, tx, q, core.DecisionSweepTierSystemDesign, documentID, confirmed.Content); err != nil {
			return err
		}
		return activatePendingTaskContextTx(ctx, tx, q, workspace(ctx), documentID, version, true)
	})
	return document, confirmed, err
}

const systemDesignSelect = `SELECT workspace_id,id,slug,title,category,current_version,created_at,updated_at FROM system_designs`
const systemDesignVersionSelect = `SELECT workspace_id,document_id,version,content,governs,origin,coalesce(origin_session_id,''),coalesce(origin_task_id,''),confirmed,coalesce(confirmed_by,''),confirmed_at,dismissed,coalesce(dismissed_by,''),dismissed_at,created_at FROM system_design_versions`

func scanSystemDesign(row pgx.Row, id string) (core.SystemDesign, error) {
	var item core.SystemDesign
	var current *int
	err := row.Scan(&item.Workspace, &item.ID, &item.Slug, &item.Title, &item.Category, &current, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, fmt.Errorf("%w: system design %s", store.ErrNotFound, id)
	}
	if current != nil {
		item.CurrentVersion = *current
	}
	return item, err
}
func scanSystemDesignVersion(row pgx.Row, id string, version int) (core.SystemDesignVersion, error) {
	var item core.SystemDesignVersion
	var raw []byte
	var origin string
	var confirmedAt *time.Time
	var dismissedAt *time.Time
	err := row.Scan(&item.Workspace, &item.DocumentID, &item.Version, &item.Content, &raw, &origin, &item.OriginSessionID, &item.OriginTaskID, &item.Confirmed, &item.ConfirmedBy, &confirmedAt, &item.Dismissed, &item.DismissedBy, &dismissedAt, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
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

func (s *Store) GetSystemDesignVersion(ctx context.Context, id string, version int) (core.SystemDesignVersion, error) {
	return scanSystemDesignVersion(s.pool.QueryRow(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND document_id=$2 AND version=$3`, workspace(ctx), id, version), id, version)
}
func (s *Store) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	rows, err := s.pool.Query(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND document_id=$2 ORDER BY version`, workspace(ctx), id)
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
	scope, _ := json.Marshal([]map[string]string{{"repository": repository}})
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.title,d.category,v.version,v.content,v.governs
		FROM system_designs d JOIN system_design_versions v
		  ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
		WHERE d.workspace_id=$1 AND v.governs @> $2::jsonb ORDER BY d.id`, workspace(ctx), scope)
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
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListPendingSystemDesignVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error) {
	rows, err := s.pool.Query(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND origin=$2 AND origin_task_id=$3 AND NOT confirmed AND NOT dismissed ORDER BY document_id,version`, workspace(ctx), string(core.SystemDesignOriginImplementation), taskID)
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
	rows, err := s.pool.Query(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 AND origin=$2 AND origin_task_id=$3 AND NOT dismissed ORDER BY document_id,version`, workspace(ctx), string(core.SystemDesignOriginImplementation), taskID)
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
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=$1 AND kind='system_design.version_proposed' AND payload_json->>'origin_task_id'=$2 ORDER BY id`, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Event, 0)
	for rows.Next() {
		var row db.Event
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.PayloadJson, &row.At, &row.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, eventFromDB(row))
	}
	return out, rows.Err()
}
func (s *Store) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	rows, err := s.pool.Query(ctx, systemDesignVersionSelect+` WHERE workspace_id=$1 ORDER BY document_id,version`, workspace(ctx))
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
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=$1 AND kind LIKE 'system_design.%' AND payload_json->>'document_id'=$2 ORDER BY id`, workspace(ctx), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var row db.Event
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.PayloadJson, &row.At, &row.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, eventFromDB(row))
	}
	return out, rows.Err()
}

func (s *Store) ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id FROM events WHERE workspace_id=$1 AND kind LIKE 'system_design.%' ORDER BY id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.Event{}
	for rows.Next() {
		var row db.Event
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.PayloadJson, &row.At, &row.WorkspaceID); err != nil {
			return nil, err
		}
		var payload map[string]any
		if json.Unmarshal(row.PayloadJson, &payload) != nil {
			continue
		}
		if documentID, _ := payload["document_id"].(string); documentID != "" {
			out[documentID] = append(out[documentID], eventFromDB(row))
		}
	}
	return out, rows.Err()
}

func (s *Store) RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM system_design_versions WHERE workspace_id=$1 AND document_id=$2 AND version=$3)
			AND CASE WHEN $5 <> '' THEN EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=$1 AND id=$5)
			ELSE EXISTS(SELECT 1 FROM planning_sessions WHERE workspace_id=$1 AND id=$4) END`,
			workspace(ctx), documentID, version, sessionID, workOrderID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: system design consultation target", store.ErrNotFound)
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.consulted", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspace(ctx), "document_id": documentID, "version": version,
			"session_id": sessionID, "work_order_id": workOrderID,
		})})
	})
}

func (s *Store) ProposeDecision(ctx context.Context, decision core.Decision) (core.Decision, error) {
	if err := core.ValidateDecision(decision); err != nil {
		return decision, err
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if decision.ID == "" {
			var high int
			if err := tx.QueryRow(ctx, `INSERT INTO decision_sequences(workspace_id,high_water_mark) VALUES($1,1) ON CONFLICT(workspace_id) DO UPDATE SET high_water_mark=decision_sequences.high_water_mark+1 RETURNING high_water_mark`, workspace(ctx)).Scan(&high); err != nil {
				return err
			}
			decision.ID = "DEC-" + strconv.Itoa(high)
		} else {
			n, _ := strconv.Atoi(strings.TrimPrefix(decision.ID, "DEC-"))
			if _, err := tx.Exec(ctx, `INSERT INTO decision_sequences(workspace_id,high_water_mark) VALUES($1,$2) ON CONFLICT(workspace_id) DO UPDATE SET high_water_mark=greatest(decision_sequences.high_water_mark,excluded.high_water_mark)`, workspace(ctx), n); err != nil {
				return err
			}
		}
		if decision.Supersedes != "" {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM decisions WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), decision.Supersedes).Scan(&status); err != nil {
				return notFound(err, "decision %s", decision.Supersedes)
			}
			if status != string(core.DecisionConfirmed) {
				return fmt.Errorf("%w: decision %s can supersede only a confirmed decision", store.ErrDecisionSupersessionConflict, decision.ID)
			}
		}
		decision.Workspace, decision.Status, decision.CreatedAt = workspace(ctx), core.DecisionProposed, time.Now().UTC()
		decision.ConfirmedBy, decision.ConfirmedAt, decision.DismissedBy, decision.DismissedAt, decision.SupersededBy = "", time.Time{}, "", time.Time{}, ""
		if _, err := tx.Exec(ctx, `INSERT INTO decisions(workspace_id,id,statement,context,alternatives_rejected,status,origin,origin_session_id,origin_task_id,supersedes,created_at) VALUES($1,$2,$3,$4,$5,'proposed',$6,$7,$8,$9,$10)`, workspace(ctx), decision.ID, decision.Statement, decision.Context, decision.AlternativesRejected, string(decision.Origin), nullString(decision.OriginSessionID), nullString(decision.OriginTaskID), nullString(decision.Supersedes), decision.CreatedAt); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "decision.proposed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "decision_id": decision.ID, "origin": decision.Origin, "origin_session_id": decision.OriginSessionID, "origin_task_id": decision.OriginTaskID, "supersedes": decision.Supersedes})})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "decisions_pkey" {
				return decision, fmt.Errorf("%w: %s", store.ErrDecisionIDConflict, decision.ID)
			}
			if pgErr.ConstraintName == "decisions_confirmed_supersedes_key" {
				return decision, fmt.Errorf("%w: %s", store.ErrDecisionSupersessionConflict, decision.Supersedes)
			}
		}
	}
	return decision, err
}

func (s *Store) ConfirmDecision(ctx context.Context, id string) (core.Decision, error) {
	var decision core.Decision
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		decision, err = scanDecision(tx.QueryRow(ctx, decisionSelect+` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id), id)
		if err != nil {
			return err
		}
		if decision.Status == core.DecisionConfirmed {
			return nil
		}
		if decision.Status != core.DecisionProposed {
			return fmt.Errorf("%w: decision %s is %s and cannot be confirmed", store.ErrDecisionSupersessionConflict, id, decision.Status)
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if decision.Supersedes != "" {
			var predecessorStatus string
			if err = tx.QueryRow(ctx, `SELECT status FROM decisions WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), decision.Supersedes).Scan(&predecessorStatus); err != nil {
				return notFound(err, "decision %s", decision.Supersedes)
			}
			if predecessorStatus != string(core.DecisionConfirmed) {
				return fmt.Errorf("%w: %s is no longer confirmed", store.ErrDecisionSupersessionConflict, decision.Supersedes)
			}
			if _, err = tx.Exec(ctx, `UPDATE decisions SET status='superseded',superseded_by=$3 WHERE workspace_id=$1 AND id=$2 AND status='confirmed'`, workspace(ctx), decision.Supersedes, id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE decisions SET status='confirmed',confirmed_by=$3,confirmed_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id, actor.ID, now); err != nil {
			return err
		}
		decision.Status, decision.ConfirmedBy, decision.ConfirmedAt = core.DecisionConfirmed, actor.ID, now
		if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: "decision.confirmed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "decision_id": id, "confirmed_by": actor.ID, "supersedes": decision.Supersedes})}); err != nil {
			return err
		}
		return recomputeDecisionSupersessionSweepTx(ctx, tx, q, decision)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "decisions_confirmed_supersedes_key" {
			return decision, fmt.Errorf("%w: %s", store.ErrDecisionSupersessionConflict, decision.Supersedes)
		}
	}
	if err == nil {
		decision, err = s.GetDecision(ctx, id)
	}
	return decision, err
}

func (s *Store) DismissDecision(ctx context.Context, id string) (core.Decision, error) {
	var decision core.Decision
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		decision, err = scanDecision(tx.QueryRow(ctx, decisionSelect+` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id), id)
		if err != nil {
			return err
		}
		if decision.Status == core.DecisionDismissed {
			return nil
		}
		if decision.Status != core.DecisionProposed {
			return fmt.Errorf("%w: decision %s is %s and cannot be dismissed", store.ErrDecisionSupersessionConflict, id, decision.Status)
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if _, err = tx.Exec(ctx, `UPDATE decisions SET status='dismissed',dismissed_by=$3,dismissed_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id, actor.ID, now); err != nil {
			return err
		}
		decision.Status, decision.DismissedBy, decision.DismissedAt = core.DecisionDismissed, actor.ID, now
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "decision.dismissed", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "decision_id": id, "dismissed_by": actor.ID, "supersedes": decision.Supersedes})})
	})
	if err == nil {
		decision, err = s.GetDecision(ctx, id)
	}
	return decision, err
}

const decisionSelect = `SELECT workspace_id,id,statement,context,alternatives_rejected,status,origin,coalesce(origin_session_id,''),coalesce(origin_task_id,''),coalesce(supersedes,''),coalesce(confirmed_by,''),confirmed_at,coalesce(dismissed_by,''),dismissed_at,coalesce(superseded_by,''),created_at FROM decisions`

func scanDecision(row pgx.Row, id string) (core.Decision, error) {
	var item core.Decision
	var status, origin string
	var confirmedAt, dismissedAt *time.Time
	err := row.Scan(&item.Workspace, &item.ID, &item.Statement, &item.Context, &item.AlternativesRejected, &status, &origin, &item.OriginSessionID, &item.OriginTaskID, &item.Supersedes, &item.ConfirmedBy, &confirmedAt, &item.DismissedBy, &dismissedAt, &item.SupersededBy, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, fmt.Errorf("%w: decision %s", store.ErrNotFound, id)
	}
	item.Status, item.Origin = core.DecisionStatus(status), core.DecisionOrigin(origin)
	if confirmedAt != nil {
		item.ConfirmedAt = *confirmedAt
	}
	if dismissedAt != nil {
		item.DismissedAt = *dismissedAt
	}
	return item, err
}
func (s *Store) GetDecision(ctx context.Context, id string) (core.Decision, error) {
	item, err := scanDecision(s.pool.QueryRow(ctx, decisionSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
	if err != nil {
		return item, err
	}
	entries, err := s.listDecisionSupersessionSweeps(ctx, id)
	item.Sweep = decisionSupersessionSweep(entries)
	return item, err
}
func (s *Store) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	rows, err := s.pool.Query(ctx, decisionSelect+` WHERE workspace_id=$1 ORDER BY substring(id from 5)::integer`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Decision{}
	for rows.Next() {
		item, scanErr := scanDecision(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	entries, err := s.listDecisionSupersessionSweeps(ctx, "")
	if err != nil {
		return nil, err
	}
	byDecision := make(map[string][]core.DecisionSupersessionSweepEntry)
	for _, entry := range entries {
		byDecision[entry.DecisionID] = append(byDecision[entry.DecisionID], entry)
	}
	for i := range out {
		out[i].Sweep = decisionSupersessionSweep(byDecision[out[i].ID])
	}
	return out, nil
}

const decisionSupersessionSweepSelect = `SELECT decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at,resolved_by,resolved_at FROM decision_supersession_sweeps`

type decisionSupersessionSweepScanner interface {
	Scan(dest ...any) error
}

func scanDecisionSupersessionSweep(row decisionSupersessionSweepScanner) (core.DecisionSupersessionSweepEntry, error) {
	var entry core.DecisionSupersessionSweepEntry
	var status string
	var resolvedAt *time.Time
	err := row.Scan(&entry.DecisionID, &entry.SupersededDecisionID, &entry.DocumentTier, &entry.DocumentID, &status, &entry.DetectedBy, &entry.DetectedAt, &entry.ResolvedBy, &resolvedAt)
	entry.Status = core.DecisionSupersessionSweepStatus(status)
	if resolvedAt != nil {
		entry.ResolvedAt = *resolvedAt
	}
	return entry, err
}

func (s *Store) listDecisionSupersessionSweeps(ctx context.Context, decisionID string) ([]core.DecisionSupersessionSweepEntry, error) {
	rows, err := s.queries.ListDecisionSupersessionSweeps(ctx, db.ListDecisionSupersessionSweepsParams{WorkspaceID: workspace(ctx), DecisionID: decisionID})
	if err != nil {
		return nil, err
	}
	entries := make([]core.DecisionSupersessionSweepEntry, 0, len(rows))
	for _, row := range rows {
		entry := core.DecisionSupersessionSweepEntry{
			DecisionID: row.DecisionID, SupersededDecisionID: row.SupersededDecisionID,
			DocumentTier: row.DocumentTier, DocumentID: row.DocumentID,
			Status: core.DecisionSupersessionSweepStatus(row.Status), DetectedBy: row.DetectedBy,
			DetectedAt: row.DetectedAt.Time, ResolvedBy: row.ResolvedBy,
		}
		if row.ResolvedAt.Valid {
			entry.ResolvedAt = row.ResolvedAt.Time
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func decisionSupersessionSweep(entries []core.DecisionSupersessionSweepEntry) core.DecisionSupersessionSweep {
	clean := true
	for _, entry := range entries {
		clean = clean && entry.Status != core.DecisionSweepOpen
	}
	if entries == nil {
		entries = []core.DecisionSupersessionSweepEntry{}
	}
	return core.DecisionSupersessionSweep{Clean: clean, Entries: entries}
}

func recomputeDecisionSupersessionSweepTx(ctx context.Context, tx pgx.Tx, q *db.Queries, decision core.Decision) error {
	if decision.Supersedes == "" {
		return nil
	}
	exists, err := decisionSupersessionSweepTableExistsTx(ctx, tx)
	if err != nil || !exists {
		return err
	}
	actor, now := store.ActorFromContext(ctx), time.Now().UTC()
	rows, err := tx.Query(ctx, `
WITH current_corpus AS (
  SELECT r.workspace_id,'requirement'::text document_tier,r.id document_id,v.content
  FROM requirements r JOIN requirement_versions v ON v.workspace_id=r.workspace_id AND v.requirement_id=r.id AND v.version=r.current_version
  WHERE r.workspace_id=$1 AND v.confirmed
  UNION ALL
  SELECT d.workspace_id,'system_design',d.id,v.content
  FROM system_designs d JOIN system_design_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
  WHERE d.workspace_id=$1 AND v.confirmed
  UNION ALL
  SELECT d.workspace_id,'reference_document',d.id,v.content
  FROM reference_documents d JOIN reference_document_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
  WHERE d.workspace_id=$1 AND d.deleted_at IS NULL
)
INSERT INTO decision_supersession_sweeps(workspace_id,decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at)
SELECT $1,$2,$3,document_tier,document_id,'open',$4,$5
FROM current_corpus WHERE content ~ ('\m' || $3 || '\M')
ON CONFLICT (workspace_id,decision_id,document_tier,document_id) DO UPDATE
SET status='open',detected_by=excluded.detected_by,detected_at=excluded.detected_at,resolved_by='',resolved_at=NULL
WHERE decision_supersession_sweeps.status='auto_cleared'
RETURNING decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at,resolved_by,resolved_at`,
		workspace(ctx), decision.ID, decision.Supersedes, actor.ID, now)
	if err != nil {
		return err
	}
	return appendDecisionSweepEvents(ctx, rows, q, "decision.supersession_sweep_opened")
}

func recomputeDecisionSweepsForDocumentTx(ctx context.Context, tx pgx.Tx, q *db.Queries, tier, documentID, content string) error {
	exists, err := decisionSupersessionSweepTableExistsTx(ctx, tx)
	if err != nil || !exists {
		return err
	}
	actor, now := store.ActorFromContext(ctx), time.Now().UTC()
	cleared, err := tx.Query(ctx, `
UPDATE decision_supersession_sweeps s
SET status='auto_cleared',resolved_by=$4,resolved_at=$5
FROM decisions d
WHERE s.workspace_id=$1 AND s.document_tier=$2 AND s.document_id=$3 AND s.status='open'
  AND d.workspace_id=s.workspace_id AND d.id=s.decision_id
  AND NOT ($6 ~ ('\m' || d.supersedes || '\M'))
RETURNING s.decision_id,s.superseded_decision_id,s.document_tier,s.document_id,s.status,s.detected_by,s.detected_at,s.resolved_by,s.resolved_at`,
		workspace(ctx), tier, documentID, actor.ID, now, content)
	if err != nil {
		return err
	}
	if err = appendDecisionSweepEvents(ctx, cleared, q, "decision.supersession_sweep_auto_cleared"); err != nil {
		return err
	}
	opened, err := tx.Query(ctx, `
INSERT INTO decision_supersession_sweeps(workspace_id,decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at)
SELECT d.workspace_id,d.id,d.supersedes,$2,$3,'open',$4,$5
FROM decisions d
WHERE d.workspace_id=$1 AND d.supersedes IS NOT NULL AND d.confirmed_at IS NOT NULL
  AND $6 ~ ('\m' || d.supersedes || '\M')
ON CONFLICT (workspace_id,decision_id,document_tier,document_id) DO UPDATE
SET status='open',detected_by=excluded.detected_by,detected_at=excluded.detected_at,resolved_by='',resolved_at=NULL
WHERE decision_supersession_sweeps.status='auto_cleared'
RETURNING decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at,resolved_by,resolved_at`,
		workspace(ctx), tier, documentID, actor.ID, now, content)
	if err != nil {
		return err
	}
	return appendDecisionSweepEvents(ctx, opened, q, "decision.supersession_sweep_opened")
}

func appendDecisionSweepEvents(ctx context.Context, rows pgx.Rows, q *db.Queries, kind string) error {
	entries := make([]core.DecisionSupersessionSweepEntry, 0)
	for rows.Next() {
		entry, err := scanDecisionSupersessionSweep(rows)
		if err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, entry)
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: kind, Payload: core.JSONPayload(entry)}); err != nil {
			return err
		}
	}
	return nil
}

func decisionSupersessionSweepTableExistsTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT to_regclass('decision_supersession_sweeps') IS NOT NULL`).Scan(&exists)
	return exists, err
}

func (s *Store) DismissDecisionSupersessionSweep(ctx context.Context, decisionID, documentTier, documentID string) (core.DecisionSupersessionSweepEntry, error) {
	var entry core.DecisionSupersessionSweepEntry
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		current, err := scanDecisionSupersessionSweep(tx.QueryRow(ctx, decisionSupersessionSweepSelect+` WHERE workspace_id=$1 AND decision_id=$2 AND document_tier=$3 AND document_id=$4 FOR UPDATE`, workspace(ctx), decisionID, documentTier, documentID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: decision sweep entry %s/%s/%s", store.ErrNotFound, decisionID, documentTier, documentID)
		}
		if err != nil {
			return err
		}
		if current.Status == core.DecisionSweepDismissed {
			entry = current
			return nil
		}
		if current.Status != core.DecisionSweepOpen {
			return fmt.Errorf("%w: entry is %s and cannot be dismissed", store.ErrDecisionSweepTransition, current.Status)
		}
		entry, err = scanDecisionSupersessionSweep(tx.QueryRow(ctx, `UPDATE decision_supersession_sweeps SET status='dismissed',resolved_by=$5,resolved_at=$6 WHERE workspace_id=$1 AND decision_id=$2 AND document_tier=$3 AND document_id=$4 RETURNING decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at,resolved_by,resolved_at`, workspace(ctx), decisionID, documentTier, documentID, actor.ID, now))
		if err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "decision.supersession_sweep_dismissed", Payload: core.JSONPayload(entry)})
	})
	return entry, err
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
