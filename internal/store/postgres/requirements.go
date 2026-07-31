package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Requirement and planning-session persistence (spec §§4.2 item 1, 9, 16;
// §21.46 change 2). Every mutation commits its projection update and audit
// event in one transaction, exactly as lifecycle transitions do (§3.3).

func (s *Store) CreateRequirement(ctx context.Context, requirement core.Requirement, first core.RequirementVersion) (core.Requirement, core.RequirementVersion, error) {
	if requirement.ID == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement id is required")
	}
	if requirement.Title == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement title is required")
	}
	if requirement.Slug == "" {
		requirement.Slug = core.RequirementSlug(requirement.Title)
	}
	if err := core.ValidateRequirementOrigin(first); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := core.ValidateRequirementStatements(first.Statements); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	now := time.Now().UTC()
	requirement.Workspace = workspace(ctx)
	// A new document is visibly pending: current_version stays NULL until an
	// operator confirms, so nothing is silently authoritative.
	requirement.CurrentVersion = 0
	requirement.StatementHighWaterMark = core.RequirementStatementHighWaterMark(first.Statements)
	if requirement.CreatedAt.IsZero() {
		requirement.CreatedAt = now
	}
	requirement.UpdatedAt = now
	first.Workspace = requirement.Workspace
	first.RequirementID = requirement.ID
	first.Version = 1
	first.Confirmed = false
	first.ConfirmedBy = ""
	first.ConfirmedAt = time.Time{}
	if first.CreatedAt.IsZero() {
		first.CreatedAt = now
	}
	statements, err := marshalRequirementStatements(first.Statements)
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO requirements
			(workspace_id,id,slug,title,current_version,statement_high_water_mark,created_at,updated_at)
			VALUES ($1,$2,$3,$4,NULL,$5,$6,$7)`,
			requirement.Workspace, requirement.ID, requirement.Slug, requirement.Title,
			requirement.StatementHighWaterMark, requirement.CreatedAt, requirement.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO requirement_versions
			(workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_drift_id,confirmed,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,false,$9)`,
			first.Workspace, first.RequirementID, first.Version, first.Content, statements,
			string(first.Origin), first.OriginSessionID, first.OriginDriftID, first.CreatedAt); err != nil {
			return err
		}
		if err := insertRequirementEvent(ctx, q, "requirement.created", map[string]any{
			"workspace_id": requirement.Workspace, "requirement_id": requirement.ID,
			"slug": requirement.Slug, "title": requirement.Title,
		}); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_proposed", map[string]any{
			"workspace_id": requirement.Workspace, "requirement_id": requirement.ID,
			"version": first.Version, "origin": first.Origin,
			"origin_session_id": first.OriginSessionID, "origin_drift_id": first.OriginDriftID,
			"statement_count": len(first.Statements),
		})
	})
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	return requirement, first, nil
}

func (s *Store) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	return scanRequirement(s.pool.QueryRow(ctx, requirementSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
}

func (s *Store) ListRequirements(ctx context.Context) ([]core.Requirement, error) {
	rows, err := s.pool.Query(ctx, requirementSelect+` WHERE workspace_id=$1 ORDER BY title,id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Requirement{}
	for rows.Next() {
		var requirement core.Requirement
		var currentVersion *int32
		if err := rows.Scan(&requirement.Workspace, &requirement.ID, &requirement.Slug, &requirement.Title,
			&currentVersion, &requirement.StatementHighWaterMark, &requirement.CreatedAt, &requirement.UpdatedAt); err != nil {
			return nil, err
		}
		if currentVersion != nil {
			requirement.CurrentVersion = int(*currentVersion)
		}
		out = append(out, requirement)
	}
	return out, rows.Err()
}

func (s *Store) ProposeRequirementVersion(ctx context.Context, version core.RequirementVersion) (core.RequirementVersion, error) {
	if err := core.ValidateRequirementOrigin(version); err != nil {
		return core.RequirementVersion{}, err
	}
	version.Workspace = workspace(ctx)
	version.Confirmed = false
	version.ConfirmedBy = ""
	version.ConfirmedAt = time.Time{}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now().UTC()
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Lock the document so concurrent proposals cannot pick the same
		// version number or race the high-water mark forward.
		var highWaterMark int
		if err := tx.QueryRow(ctx,
			`SELECT statement_high_water_mark FROM requirements
			 WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			version.Workspace, version.RequirementID).Scan(&highWaterMark); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("requirement %s not found", version.RequirementID)
			}
			return err
		}
		// Every REQ-n the document has ever issued, not just its latest
		// version's, so reinstating a statement that an unconfirmed proposal
		// dropped is not mistaken for identifier reuse.
		var latestVersion int
		var issued []string
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(version), 0),
			        coalesce(array_agg(DISTINCT statement.id) FILTER (WHERE statement.id IS NOT NULL), '{}')
			 FROM requirement_versions
			 LEFT JOIN LATERAL jsonb_to_recordset(statements_json) AS statement(id text) ON true
			 WHERE workspace_id=$1 AND requirement_id=$2`,
			version.Workspace, version.RequirementID).Scan(&latestVersion, &issued); err != nil {
			return err
		}
		if err := core.ValidateRequirementRevision(highWaterMark, issued, version.Statements); err != nil {
			return err
		}
		version.Version = latestVersion + 1
		statements, err := marshalRequirementStatements(version.Statements)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO requirement_versions
			(workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_drift_id,confirmed,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,false,$9)`,
			version.Workspace, version.RequirementID, version.Version, version.Content, statements,
			string(version.Origin), version.OriginSessionID, version.OriginDriftID, version.CreatedAt); err != nil {
			return err
		}
		if mark := core.RequirementStatementHighWaterMark(version.Statements); mark > highWaterMark {
			highWaterMark = mark
		}
		if _, err := tx.Exec(ctx,
			`UPDATE requirements SET statement_high_water_mark=$3, updated_at=now()
			 WHERE workspace_id=$1 AND id=$2`,
			version.Workspace, version.RequirementID, highWaterMark); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_proposed", map[string]any{
			"workspace_id": version.Workspace, "requirement_id": version.RequirementID,
			"version": version.Version, "origin": version.Origin,
			"origin_session_id": version.OriginSessionID, "origin_drift_id": version.OriginDriftID,
			"statement_count": len(version.Statements),
		})
	})
	if err != nil {
		return core.RequirementVersion{}, err
	}
	return version, nil
}

func (s *Store) ConfirmRequirementVersion(ctx context.Context, requirementID string, version int) (core.Requirement, core.RequirementVersion, error) {
	var (
		requirement core.Requirement
		confirmed   core.RequirementVersion
	)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var currentVersion *int32
		var highWaterMark int
		if err := tx.QueryRow(ctx,
			`SELECT current_version, statement_high_water_mark FROM requirements
			 WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			workspace(ctx), requirementID).Scan(&currentVersion, &highWaterMark); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("requirement %s not found", requirementID)
			}
			return err
		}
		stored, err := scanRequirementVersion(tx.QueryRow(ctx, requirementVersionSelect+
			` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version), requirementID, version)
		if err != nil {
			return err
		}
		if stored.Confirmed {
			confirmed = stored
			requirement, err = scanRequirement(tx.QueryRow(ctx, requirementSelect+
				` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), requirementID), requirementID)
			return err
		}
		// Confirmation is where a real statement block becomes mandatory, so a
		// migration seed cannot become current intent unedited.
		if err := core.ConfirmableRequirementVersion(stored); err != nil {
			return err
		}
		// Confirmation moves forward only: re-confirming a superseded version
		// would silently revert intent the operator already advanced past.
		if currentVersion != nil && version < int(*currentVersion) {
			return fmt.Errorf("requirement %s already confirmed version %d; cannot confirm earlier version %d", requirementID, *currentVersion, version)
		}
		actor := store.ActorFromContext(ctx)
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx,
			`UPDATE requirement_versions SET confirmed=true, confirmed_by=$4, confirmed_at=$5
			 WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version, actor.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE requirements SET current_version=$3, updated_at=$4
			 WHERE workspace_id=$1 AND id=$2`,
			workspace(ctx), requirementID, version, now); err != nil {
			return err
		}
		stored.Confirmed, stored.ConfirmedBy, stored.ConfirmedAt = true, actor.ID, now
		confirmed = stored
		if requirement, err = scanRequirement(tx.QueryRow(ctx, requirementSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), requirementID), requirementID); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_confirmed", map[string]any{
			"workspace_id": workspace(ctx), "requirement_id": requirementID,
			"version": version, "origin": stored.Origin, "confirmed_by": actor.ID,
		})
	})
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	return requirement, confirmed, nil
}

func (s *Store) GetRequirementVersion(ctx context.Context, requirementID string, version int) (core.RequirementVersion, error) {
	return scanRequirementVersion(s.pool.QueryRow(ctx, requirementVersionSelect+
		` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
		workspace(ctx), requirementID, version), requirementID, version)
}

func (s *Store) ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error) {
	rows, err := s.pool.Query(ctx, requirementVersionSelect+
		` WHERE workspace_id=$1 AND requirement_id=$2 ORDER BY version`, workspace(ctx), requirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.RequirementVersion{}
	for rows.Next() {
		version, err := scanRequirementVersionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, version)
	}
	return out, rows.Err()
}

func (s *Store) CreatePlanningSession(ctx context.Context, session core.PlanningSession) (core.PlanningSession, error) {
	if session.ID == "" {
		return core.PlanningSession{}, fmt.Errorf("planning session id is required")
	}
	now := time.Now().UTC()
	session.Workspace = workspace(ctx)
	session.Status = core.PlanningSessionActive
	session.ProducedRequirementID = ""
	session.ProducedTaskID = ""
	session.TranscriptArtifactID = ""
	session.FinalizedAt = time.Time{}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	if session.PinnedRevisions == nil {
		session.PinnedRevisions = map[string]string{}
	}
	pins, err := json.Marshal(session.PinnedRevisions)
	if err != nil {
		return core.PlanningSession{}, fmt.Errorf("encode planning revisions: %w", err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO planning_sessions
			(workspace_id,id,title,status,requirement_context_id,model,effort,
			 exploration_output_tokens,exploration_tokens_used,primary_repo,pinned_revisions,
			 created_at,updated_at)
			VALUES ($1,$2,$3,'active',NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12)`,
			session.Workspace, session.ID, session.Title, session.RequirementContextID,
			session.Model, session.Effort, session.ExplorationOutputTokens,
			session.ExplorationTokensUsed, session.PrimaryRepo, pins,
			session.CreatedAt, session.UpdatedAt); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "planning_session.created", map[string]any{
			"workspace_id": session.Workspace, "session_id": session.ID, "title": session.Title,
			"requirement_context_id": session.RequirementContextID,
			"model":                  session.Model, "effort": session.Effort,
			"exploration_output_tokens": session.ExplorationOutputTokens,
			"primary_repo":              session.PrimaryRepo, "pinned_revisions": session.PinnedRevisions,
		})
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

func (s *Store) GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error) {
	return scanPlanningSession(s.pool.QueryRow(ctx, planningSessionSelect+
		` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
}

func (s *Store) ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error) {
	rows, err := s.pool.Query(ctx, planningSessionSelect+
		` WHERE workspace_id=$1 ORDER BY updated_at DESC, id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.PlanningSession{}
	for rows.Next() {
		session, err := scanPlanningSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *Store) PinPlanningSessionRepo(ctx context.Context, sessionID, repo, revision string) (core.PlanningSession, error) {
	var session core.PlanningSession
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		existing, err := scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), sessionID), sessionID)
		if err != nil {
			return err
		}
		if existing.Status != core.PlanningSessionActive {
			return fmt.Errorf("planning session %s is %s and cannot pin repositories", sessionID, existing.Status)
		}
		if pinned := existing.PinnedRevisions[repo]; pinned != "" {
			session = existing
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE planning_sessions
			SET pinned_revisions=jsonb_set(pinned_revisions, ARRAY[$3], to_jsonb($4::text), true),
			    updated_at=now()
			WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID, repo, revision); err != nil {
			return err
		}
		if err = insertRequirementEvent(ctx, q, "planning_session.repo_pinned", map[string]any{
			"workspace_id": workspace(ctx), "session_id": sessionID,
			"repo": repo, "revision": revision,
		}); err != nil {
			return err
		}
		session, err = scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID), sessionID)
		return err
	})
	return session, err
}

func (s *Store) RecordPlanningExplorationTokens(ctx context.Context, sessionID string, tokens int) (core.PlanningSession, error) {
	if tokens < 0 {
		return core.PlanningSession{}, fmt.Errorf("planning exploration tokens must not be negative")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE planning_sessions
		SET exploration_tokens_used=exploration_tokens_used+$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID, tokens); err != nil {
		return core.PlanningSession{}, err
	}
	return s.GetPlanningSession(ctx, sessionID)
}

func (s *Store) AppendPlanningMessage(ctx context.Context, message core.PlanningMessage) (core.PlanningMessage, error) {
	if !message.Role.Valid() {
		return core.PlanningMessage{}, fmt.Errorf("invalid planning message role %q", message.Role)
	}
	message.Workspace = workspace(ctx)
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	if len(message.Parts) == 0 {
		message.Parts = json.RawMessage(`[]`)
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Lock the session so concurrent appends cannot claim the same sequence.
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM planning_sessions WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			message.Workspace, message.SessionID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("planning session %s not found", message.SessionID)
			}
			return err
		}
		if core.PlanningSessionStatus(status) != core.PlanningSessionActive {
			return fmt.Errorf("planning session %s is %s and accepts no further messages", message.SessionID, status)
		}
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(max(seq),0)+1 FROM planning_messages
			 WHERE workspace_id=$1 AND session_id=$2`,
			message.Workspace, message.SessionID).Scan(&message.Seq); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO planning_messages
			(workspace_id,session_id,seq,role,content,parts_json,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			message.Workspace, message.SessionID, message.Seq, string(message.Role),
			message.Content, []byte(message.Parts), message.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE planning_sessions SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`,
			message.Workspace, message.SessionID, message.CreatedAt); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "planning_session.message_appended", map[string]any{
			"workspace_id": message.Workspace, "session_id": message.SessionID,
			"seq": message.Seq, "role": message.Role,
		})
	})
	if err != nil {
		return core.PlanningMessage{}, err
	}
	return message, nil
}

func (s *Store) ListPlanningMessages(ctx context.Context, sessionID string) ([]core.PlanningMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT workspace_id,session_id,seq,role,content,parts_json,created_at
		 FROM planning_messages WHERE workspace_id=$1 AND session_id=$2 ORDER BY seq`,
		workspace(ctx), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.PlanningMessage{}
	for rows.Next() {
		var message core.PlanningMessage
		var role string
		var parts []byte
		if err := rows.Scan(&message.Workspace, &message.SessionID, &message.Seq, &role,
			&message.Content, &parts, &message.CreatedAt); err != nil {
			return nil, err
		}
		message.Role = core.PlanningMessageRole(role)
		message.Parts = json.RawMessage(parts)
		out = append(out, message)
	}
	return out, rows.Err()
}

func (s *Store) FinalizePlanningSession(ctx context.Context, request store.PlanningFinalizeRequest) (core.PlanningSession, error) {
	if err := request.Validate(); err != nil {
		return core.PlanningSession{}, err
	}
	var session core.PlanningSession
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		existing, err := scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), request.SessionID), request.SessionID)
		if err != nil {
			return err
		}
		if existing.Status == core.PlanningSessionFinalized {
			// Idempotent for an identical finalize; any difference in the
			// recorded lineage — produced artifact or archived transcript — is
			// a contradiction, not a retry, so the stored lineage stands.
			if existing.ProducedRequirementID == request.RequirementID &&
				existing.ProducedTaskID == request.TaskID &&
				existing.TranscriptArtifactID == request.TranscriptArtifactID {
				session = existing
				return nil
			}
			return fmt.Errorf(
				"planning session %s is already finalized with different lineage", request.SessionID)
		}
		if existing.Status != core.PlanningSessionActive {
			// The row lock serializes finalize against abandon. When abandon
			// wins, its terminal state must not be overwritten by the
			// in-flight planning run (spec §9).
			return fmt.Errorf(
				"planning session %s is %s and cannot be finalized", request.SessionID, existing.Status)
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE planning_sessions
			SET status='finalized', produced_requirement_id=NULLIF($3,''),
			    produced_task_id=NULLIF($4,''), transcript_artifact_id=NULLIF($5,''),
			    finalized_at=$6, updated_at=$6
			WHERE workspace_id=$1 AND id=$2`,
			workspace(ctx), request.SessionID, request.RequirementID, request.TaskID,
			request.TranscriptArtifactID, now); err != nil {
			return err
		}
		if session, err = scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), request.SessionID), request.SessionID); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "planning_session.finalized", map[string]any{
			"workspace_id": workspace(ctx), "session_id": session.ID,
			"produced_requirement_id": session.ProducedRequirementID,
			"produced_task_id":        session.ProducedTaskID,
			"transcript_artifact_id":  session.TranscriptArtifactID,
		})
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

func (s *Store) AbandonPlanningSession(ctx context.Context, sessionID string) (core.PlanningSession, error) {
	var session core.PlanningSession
	err := s.withPlanningSessionLock(ctx, sessionID, func(lockedCtx context.Context) error {
		return s.inTx(lockedCtx, func(tx pgx.Tx, q *db.Queries) error {
			existing, err := scanPlanningSession(tx.QueryRow(lockedCtx, planningSessionSelect+
				` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(lockedCtx), sessionID), sessionID)
			if err != nil {
				return err
			}
			if existing.Status == core.PlanningSessionAbandoned {
				session = existing
				return nil
			}
			if existing.Status == core.PlanningSessionFinalized {
				// Abandoning would strand what the session produced.
				return fmt.Errorf("planning session %s is already finalized", sessionID)
			}
			if _, err := tx.Exec(lockedCtx, `UPDATE planning_sessions
				SET status='abandoned', updated_at=$3
				WHERE workspace_id=$1 AND id=$2`,
				workspace(lockedCtx), sessionID, time.Now().UTC()); err != nil {
				return err
			}
			if session, err = scanPlanningSession(tx.QueryRow(lockedCtx, planningSessionSelect+
				` WHERE workspace_id=$1 AND id=$2`, workspace(lockedCtx), sessionID), sessionID); err != nil {
				return err
			}
			return insertRequirementEvent(lockedCtx, q, "planning_session.abandoned", map[string]any{
				"workspace_id": workspace(lockedCtx), "session_id": session.ID,
			})
		})
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

const requirementSelect = `SELECT workspace_id,id,slug,title,current_version,statement_high_water_mark,created_at,updated_at FROM requirements`

const requirementVersionSelect = `SELECT workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_drift_id,confirmed,confirmed_by,confirmed_at,created_at FROM requirement_versions`

const planningSessionSelect = `SELECT workspace_id,id,title,status,COALESCE(requirement_context_id,''),
	COALESCE(produced_requirement_id,''),COALESCE(produced_task_id,''),
	COALESCE(transcript_artifact_id,''),model,effort,exploration_output_tokens,
	exploration_tokens_used,primary_repo,pinned_revisions,created_at,updated_at,finalized_at
	FROM planning_sessions`

func scanRequirement(row pgx.Row, id string) (core.Requirement, error) {
	var requirement core.Requirement
	var currentVersion *int32
	if err := row.Scan(&requirement.Workspace, &requirement.ID, &requirement.Slug, &requirement.Title,
		&currentVersion, &requirement.StatementHighWaterMark, &requirement.CreatedAt, &requirement.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Requirement{}, fmt.Errorf("requirement %s not found", id)
		}
		return core.Requirement{}, err
	}
	if currentVersion != nil {
		requirement.CurrentVersion = int(*currentVersion)
	}
	return requirement, nil
}

func scanRequirementVersion(row pgx.Row, requirementID string, version int) (core.RequirementVersion, error) {
	stored, err := scanRequirementVersionRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.RequirementVersion{}, fmt.Errorf("requirement %s has no version %d", requirementID, version)
	}
	return stored, err
}

func scanRequirementVersionRow(row pgx.Row) (core.RequirementVersion, error) {
	var stored core.RequirementVersion
	var origin string
	var statements []byte
	var confirmedBy string
	var confirmedAt *time.Time
	if err := row.Scan(&stored.Workspace, &stored.RequirementID, &stored.Version, &stored.Content,
		&statements, &origin, &stored.OriginSessionID, &stored.OriginDriftID,
		&stored.Confirmed, &confirmedBy, &confirmedAt, &stored.CreatedAt); err != nil {
		return core.RequirementVersion{}, err
	}
	parsed, err := unmarshalRequirementStatements(statements)
	if err != nil {
		return core.RequirementVersion{}, err
	}
	stored.Statements = parsed
	stored.Origin = core.RequirementOrigin(origin)
	stored.ConfirmedBy = confirmedBy
	if confirmedAt != nil {
		stored.ConfirmedAt = *confirmedAt
	}
	return stored, nil
}

func scanPlanningSession(row pgx.Row, id string) (core.PlanningSession, error) {
	session, err := scanPlanningSessionRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", id)
	}
	return session, err
}

func scanPlanningSessionRow(row pgx.Row) (core.PlanningSession, error) {
	var session core.PlanningSession
	var status string
	var finalizedAt *time.Time
	var pins []byte
	if err := row.Scan(&session.Workspace, &session.ID, &session.Title, &status,
		&session.RequirementContextID, &session.ProducedRequirementID, &session.ProducedTaskID,
		&session.TranscriptArtifactID, &session.Model, &session.Effort,
		&session.ExplorationOutputTokens, &session.ExplorationTokensUsed,
		&session.PrimaryRepo, &pins, &session.CreatedAt, &session.UpdatedAt, &finalizedAt); err != nil {
		return core.PlanningSession{}, err
	}
	if err := json.Unmarshal(pins, &session.PinnedRevisions); err != nil {
		return core.PlanningSession{}, fmt.Errorf("decode planning revisions: %w", err)
	}
	session.Status = core.PlanningSessionStatus(status)
	if finalizedAt != nil {
		session.FinalizedAt = *finalizedAt
	}
	return session, nil
}

func marshalRequirementStatements(statements []core.RequirementStatement) ([]byte, error) {
	if statements == nil {
		statements = []core.RequirementStatement{}
	}
	encoded, err := json.Marshal(statements)
	if err != nil {
		return nil, fmt.Errorf("encode requirement statements: %w", err)
	}
	return encoded, nil
}

func unmarshalRequirementStatements(raw []byte) ([]core.RequirementStatement, error) {
	if len(raw) == 0 {
		return []core.RequirementStatement{}, nil
	}
	statements := []core.RequirementStatement{}
	if err := json.Unmarshal(raw, &statements); err != nil {
		return nil, fmt.Errorf("decode requirement statements: %w", err)
	}
	return statements, nil
}

// insertRequirementEvent records a workspace-scoped audit event. Requirement
// and planning mutations carry no task, so they use the workspace event path
// and the migration 046 scope allowlist.
func insertRequirementEvent(ctx context.Context, q *db.Queries, kind string, payload map[string]any) error {
	actor := store.ActorFromContext(ctx)
	_, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
		WorkspaceID: workspace(ctx), Kind: kind, ActorID: actor.ID,
		ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(payload),
		At: timestamp(time.Now().UTC()),
	})
	return err
}
