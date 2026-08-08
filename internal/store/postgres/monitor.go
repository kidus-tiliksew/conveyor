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
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) AuditTask(ctx context.Context, taskID, kind string, payload map[string]any) error {
	return s.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, Payload: core.JSONPayload(payload)})
}

func (s *Store) AuditMonitor(ctx context.Context, kind string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO monitor_activity (workspace_id,kind,payload_json)
VALUES ($1,$2,$3::jsonb)`, workspace(ctx), kind, string(data))
	return err
}

func (s *Store) RequirementExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM requirements WHERE workspace_id=$1 AND id=$2
	)`, workspace(ctx), id).Scan(&exists)
	return exists, err
}

func (s *Store) ClaimCausalSystemDesignProposal(ctx context.Context, documentID, repository, commitSHA string, causalEventID int64, driftID string, matchingPaths []string) (monitor.ProposalSuppression, bool, error) {
	if causalEventID <= 0 || commitSHA == "" {
		return monitor.ProposalSuppression{}, false, nil
	}
	var result monitor.ProposalSuppression
	valid := false
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var causalTaskID string
		if err := tx.QueryRow(ctx, `SELECT causal.task_id FROM events causal
			JOIN tasks causal_task ON causal_task.workspace_id=causal.workspace_id AND causal_task.id=causal.task_id
			WHERE causal.workspace_id=$1 AND causal.id=$2 AND causal_task.repo_name=$3
			AND causal.kind IN ('merge.confirmed','merge.reconciled')
			AND causal.payload_json->>'head_sha'=$4`, workspace(ctx), causalEventID, repository, commitSHA).Scan(&causalTaskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		valid = true
		var eventID int64
		var version int
		var confirmed bool
		if err := tx.QueryRow(ctx, `SELECT proposal.id,(proposal.payload_json->>'version')::integer,version.confirmed
			FROM events proposal
			JOIN system_design_versions version ON version.workspace_id=proposal.workspace_id
			 AND version.document_id=proposal.payload_json->>'document_id'
			 AND version.version=(proposal.payload_json->>'version')::integer
			WHERE proposal.workspace_id=$1 AND proposal.id<$2 AND proposal.kind='system_design.version_proposed'
			 AND proposal.payload_json->>'document_id'=$3 AND proposal.payload_json->>'origin_task_id'=$4
			ORDER BY proposal.id DESC LIMIT 1`, workspace(ctx), causalEventID, documentID, causalTaskID).Scan(&eventID, &version, &confirmed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		lockKey := "conveyor:design-proposal:" + strconv.FormatInt(eventID, 10)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
			return err
		}
		var existingDrift, existingStatus string
		err := tx.QueryRow(ctx, `SELECT payload_json->>'drift_id',payload_json->>'proposal_status'
			FROM monitor_activity WHERE workspace_id=$1 AND kind='system_design.drift_suppressed'
			 AND payload_json->>'proposal_event_id'=$2 ORDER BY id LIMIT 1`, workspace(ctx), strconv.FormatInt(eventID, 10)).Scan(&existingDrift, &existingStatus)
		if err == nil {
			if existingDrift == driftID {
				result = monitor.ProposalSuppression{EventID: eventID, Status: existingStatus}
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		status := "pending"
		if confirmed {
			status = "confirmed"
		}
		payload, err := json.Marshal(map[string]any{"document_id": documentID, "drift_id": driftID, "merge_event_id": causalEventID, "proposal_event_id": eventID, "proposal_status": status, "proposal_version": version, "matching_paths": matchingPaths})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_activity(workspace_id,kind,payload_json) VALUES($1,'system_design.drift_suppressed',$2::jsonb)`, workspace(ctx), string(payload)); err != nil {
			return err
		}
		result = monitor.ProposalSuppression{EventID: eventID, Status: status}
		return nil
	})
	return result, valid, err
}

func (s *Store) Observe(ctx context.Context, observation monitor.Observation) (monitor.ObservationRecord, bool, error) {
	if observation.ChangedPaths == nil {
		observation.ChangedPaths = []string{}
	}
	contextJSON, err := json.Marshal(observation.Context)
	if err != nil {
		return monitor.ObservationRecord{}, false, err
	}
	var hints any
	if observation.Hints != nil {
		raw, marshalErr := json.Marshal(observation.Hints)
		if marshalErr != nil {
			return monitor.ObservationRecord{}, false, marshalErr
		}
		hints = string(raw)
	}
	identity := observation.Identity()
	tag, err := s.pool.Exec(ctx, `
INSERT INTO monitor_observations (
 workspace_id,identity,repository,kind,occurrence_id,source_url,commit_sha,
 pull_request_number,check_run_id,requirement_id,changed_paths,causal_event_id,observed_at,context_json,hint_context_json,created_at,updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14::jsonb,$15::jsonb,$13,$13)
ON CONFLICT (workspace_id,identity) DO NOTHING`,
		workspace(ctx), identity, observation.Repository, observation.Kind, observation.OccurrenceID,
		observation.SourceURL, observation.CommitSHA, observation.PullRequestNumber, observation.CheckRunID,
		observation.RequirementID, observation.ChangedPaths, nullableMonitorEventID(observation.CausalEventID), observation.ObservedAt, string(contextJSON), hints)
	if err != nil {
		return monitor.ObservationRecord{}, false, err
	}
	fresh := tag.RowsAffected() == 1
	if !fresh {
		if _, err = s.pool.Exec(ctx, `
UPDATE monitor_observations SET deduplicated_count=deduplicated_count+1,
 changed_paths=CASE WHEN cardinality($4::text[])>0 THEN $4 ELSE changed_paths END,
 causal_event_id=COALESCE($5,causal_event_id),
 state='deduplicated', updated_at=$3
WHERE workspace_id=$1 AND identity=$2`, workspace(ctx), identity, observation.ObservedAt, observation.ChangedPaths, nullableMonitorEventID(observation.CausalEventID)); err != nil {
			return monitor.ObservationRecord{}, false, err
		}
	}
	record, err := s.getObservation(ctx, identity)
	return record, fresh, err
}

func (s *Store) getObservation(ctx context.Context, identity string) (monitor.ObservationRecord, error) {
	var record monitor.ObservationRecord
	var kind string
	var contextJSON []byte
	var hintsJSON []byte
	var taskID *string
	err := s.pool.QueryRow(ctx, `
SELECT repository,kind,occurrence_id,source_url,commit_sha,pull_request_number,check_run_id,
	 COALESCE(requirement_id,''),changed_paths,COALESCE(causal_event_id,0),observed_at,context_json,COALESCE(hint_context_json,'null'::jsonb),task_id,task_outcome,state,
 deduplicated_count,forge_error_category,last_error,created_at,updated_at
FROM monitor_observations WHERE workspace_id=$1 AND identity=$2`, workspace(ctx), identity).
		Scan(&record.Repository, &kind, &record.OccurrenceID, &record.SourceURL, &record.CommitSHA,
			&record.PullRequestNumber, &record.CheckRunID, &record.RequirementID, &record.ChangedPaths, &record.CausalEventID, &record.ObservedAt,
			&contextJSON, &hintsJSON, &taskID, &record.TaskOutcome, &record.State, &record.DeduplicatedCount,
			&record.ForgeErrorCategory, &record.LastError, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return monitor.ObservationRecord{}, err
	}
	record.WorkspaceID, record.Kind = workspace(ctx), monitor.SignalKind(kind)
	if taskID != nil {
		record.TaskID = *taskID
	}
	_ = json.Unmarshal(contextJSON, &record.Context)
	if string(hintsJSON) != "null" {
		record.Hints = &monitor.HintContext{}
		_ = json.Unmarshal(hintsJSON, record.Hints)
	}
	return record, nil
}

func (s *Store) LinkTask(ctx context.Context, identity, taskID, outcome string) (monitor.ObservationRecord, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE monitor_observations SET task_id=$3,task_outcome=$4,state='task_linked',updated_at=now()
WHERE workspace_id=$1 AND identity=$2 AND (task_id IS NULL OR task_id=$3)`,
		workspace(ctx), identity, taskID, outcome)
	if err != nil {
		return monitor.ObservationRecord{}, err
	}
	if tag.RowsAffected() == 0 {
		return monitor.ObservationRecord{}, fmt.Errorf("monitor observation %s missing or linked to another task", identity)
	}
	return s.getObservation(ctx, identity)
}

func (s *Store) RecordDrift(ctx context.Context, drift monitor.Drift) (monitor.Drift, bool, error) {
	if drift.MatchingPaths == nil {
		drift.MatchingPaths = []string{}
	}
	matchingPaths, err := json.Marshal(drift.MatchingPaths)
	if err != nil {
		return monitor.Drift{}, false, err
	}
	fresh := false
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		tag, insertErr := tx.Exec(ctx, `
INSERT INTO repository_drift (
 workspace_id,id,repository,kind,source_url,commit_sha,requirement_id,system_design_id,system_design_version,causal_event_id,matching_paths,task_id,detected_at
) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,0),NULLIF($10,0),$11,$12,$13)
ON CONFLICT (workspace_id,id) DO NOTHING`,
			workspace(ctx), drift.ID, drift.Repository, drift.Kind, drift.SourceURL,
			drift.CommitSHA, drift.RequirementID, drift.SystemDesignID, drift.SystemDesignVersion, drift.CausalEventID, matchingPaths, drift.TaskID, drift.DetectedAt)
		if insertErr != nil {
			return insertErr
		}
		fresh = tag.RowsAffected() == 1
		if fresh && drift.SystemDesignID != "" {
			return insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.drift_detected", At: drift.DetectedAt, Payload: core.JSONPayload(map[string]any{
				"workspace_id": workspace(ctx), "document_id": drift.SystemDesignID, "version": drift.SystemDesignVersion,
				"drift_id": drift.ID, "causal_event_id": drift.CausalEventID, "matching_paths": drift.MatchingPaths,
			})})
		}
		return nil
	})
	if err != nil {
		return monitor.Drift{}, false, err
	}
	current, err := s.getDrift(ctx, drift.ID)
	return current, fresh, err
}

func (s *Store) getDrift(ctx context.Context, id string) (monitor.Drift, error) {
	var drift monitor.Drift
	var kind string
	var resolvedAt *time.Time
	err := s.pool.QueryRow(ctx, `
SELECT id,repository,kind,source_url,commit_sha,COALESCE(requirement_id,''),COALESCE(system_design_id,''),COALESCE(system_design_version,0),COALESCE(causal_event_id,0),matching_paths,task_id,detected_at,resolved_at,outcome
FROM repository_drift WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id).
		Scan(&drift.ID, &drift.Repository, &kind, &drift.SourceURL, &drift.CommitSHA,
			&drift.RequirementID, &drift.SystemDesignID, &drift.SystemDesignVersion, &drift.CausalEventID, &drift.MatchingPaths, &drift.TaskID, &drift.DetectedAt, &resolvedAt, &drift.Outcome)
	if err != nil {
		return monitor.Drift{}, err
	}
	drift.WorkspaceID, drift.Kind = workspace(ctx), monitor.SignalKind(kind)
	if resolvedAt != nil {
		drift.ResolvedAt = *resolvedAt
	}
	return drift, nil
}

func (s *Store) ResolveDrift(ctx context.Context, id, outcome, requirementID string) (monitor.Drift, error) {
	requirementID = strings.TrimSpace(requirementID)
	if outcome != "requirements_amended" && outcome != "design_document_updated" && outcome != "conflict_resolved" && outcome != "change_reverted" {
		return monitor.Drift{}, fmt.Errorf("unsupported audited reconciliation outcome %q", outcome)
	}
	if outcome != "requirements_amended" && requirementID != "" {
		return monitor.Drift{}, fmt.Errorf("%w: outcome %s", monitor.ErrRequirementIDNotAllowed, outcome)
	}
	var drift monitor.Drift
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var kind string
		var resolvedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT id,repository,kind,source_url,commit_sha,
			COALESCE(requirement_id,''),COALESCE(system_design_id,''),COALESCE(system_design_version,0),COALESCE(causal_event_id,0),matching_paths,task_id,detected_at,resolved_at,outcome
			FROM repository_drift WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id).
			Scan(&drift.ID, &drift.Repository, &kind, &drift.SourceURL, &drift.CommitSHA,
				&drift.RequirementID, &drift.SystemDesignID, &drift.SystemDesignVersion, &drift.CausalEventID, &drift.MatchingPaths, &drift.TaskID, &drift.DetectedAt, &resolvedAt, &drift.Outcome); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("drift %s not found", id)
			}
			return err
		}
		drift.WorkspaceID, drift.Kind = workspace(ctx), monitor.SignalKind(kind)
		if resolvedAt != nil {
			drift.ResolvedAt = *resolvedAt
			if requirementID != "" && requirementID != drift.RequirementID {
				return fmt.Errorf("%w: drift %s is linked to requirement %s", monitor.ErrRequirementIDInvalid, id, drift.RequirementID)
			}
			return nil
		}
		now := time.Now().UTC()
		if outcome == "requirements_amended" {
			if requirementID != "" && drift.RequirementID != "" && requirementID != drift.RequirementID {
				return fmt.Errorf("%w: drift %s is already linked to requirement %s", monitor.ErrRequirementIDInvalid, id, drift.RequirementID)
			}
			if drift.RequirementID == "" {
				drift.RequirementID = requirementID
			}
			if drift.RequirementID == "" {
				return fmt.Errorf("%w: drift %s cannot resolve as requirements_amended", monitor.ErrRequirementIDMissing, id)
			}
			var currentVersion *int32
			var highWaterMark int
			if err := tx.QueryRow(ctx, `SELECT current_version,statement_high_water_mark FROM requirements
				WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), drift.RequirementID).
				Scan(&currentVersion, &highWaterMark); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: %s", monitor.ErrUnknownRequirementID, drift.RequirementID)
				}
				return err
			}
			if currentVersion == nil {
				return fmt.Errorf("%w: requirement %s has no confirmed current version", monitor.ErrRequirementIDInvalid, drift.RequirementID)
			}
			current, err := scanRequirementVersion(tx.QueryRow(ctx, requirementVersionSelect+
				` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`, workspace(ctx), drift.RequirementID, *currentVersion), drift.RequirementID, int(*currentVersion))
			if err != nil {
				return err
			}
			if !current.Confirmed {
				return fmt.Errorf("%w: requirement %s current version is not confirmed", monitor.ErrRequirementIDInvalid, drift.RequirementID)
			}
			proposal, err := store.DriftAmendmentVersion(drift, current)
			if err != nil {
				return err
			}
			var latestVersion int
			var issued []string
			if err = tx.QueryRow(ctx, `SELECT coalesce(max(rv.version),0),
				coalesce(array_agg(DISTINCT ids.id) FILTER (WHERE ids.id IS NOT NULL),'{}')
				FROM requirement_versions rv
				LEFT JOIN LATERAL jsonb_array_elements(rv.statements_json) statement ON true
				LEFT JOIN LATERAL (
				  SELECT statement->>'id' AS id
				  UNION ALL
				  SELECT criterion->>'id' FROM jsonb_array_elements(coalesce(statement->'acceptance_criteria','[]'::jsonb)) criterion
				) ids ON true
				WHERE rv.workspace_id=$1 AND rv.requirement_id=$2`, workspace(ctx), drift.RequirementID).
				Scan(&latestVersion, &issued); err != nil {
				return err
			}
			if err = core.ValidateRequirementRevision(highWaterMark, issued, proposal.Statements); err != nil {
				return err
			}
			statements, err := marshalRequirementStatements(proposal.Statements)
			if err != nil {
				return err
			}
			proposal.Workspace, proposal.Version, proposal.CreatedAt = workspace(ctx), latestVersion+1, now
			if _, err = tx.Exec(ctx, `INSERT INTO requirement_versions
				(workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_drift_id,confirmed,created_at)
				VALUES ($1,$2,$3,$4,$5,$6,'',$7,false,$8)`, workspace(ctx), drift.RequirementID,
				proposal.Version, proposal.Content, statements, string(proposal.Origin), drift.ID, now); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE requirements SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), drift.RequirementID, now); err != nil {
				return err
			}
			if err = insertRequirementEvent(ctx, q, "requirement.version_proposed", map[string]any{
				"workspace_id": workspace(ctx), "requirement_id": drift.RequirementID,
				"version": proposal.Version, "origin": proposal.Origin,
				"origin_drift_id": drift.ID, "statement_count": len(proposal.Statements),
			}); err != nil {
				return err
			}
		}
		if outcome == "design_document_updated" {
			if drift.SystemDesignID == "" {
				return fmt.Errorf("drift %s cannot resolve as design_document_updated without a system design", id)
			}
			var currentVersion *int
			if err := tx.QueryRow(ctx, `SELECT current_version FROM system_designs WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), drift.SystemDesignID).Scan(&currentVersion); err != nil {
				return err
			}
			if currentVersion == nil || *currentVersion <= drift.SystemDesignVersion {
				return fmt.Errorf("drift %s requires a confirmed replacement version for system design %s", id, drift.SystemDesignID)
			}
			var confirmed bool
			if err := tx.QueryRow(ctx, `SELECT confirmed FROM system_design_versions WHERE workspace_id=$1 AND document_id=$2 AND version=$3`, workspace(ctx), drift.SystemDesignID, *currentVersion).Scan(&confirmed); err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("drift %s requires a confirmed replacement version for system design %s", id, drift.SystemDesignID)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE repository_drift SET requirement_id=NULLIF($3,''),resolved_at=$4,outcome=$5
			WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id, drift.RequirementID, now, outcome); err != nil {
			return err
		}
		drift.Outcome, drift.ResolvedAt = outcome, now
		if err := insertEvent(ctx, q, core.Event{TaskID: drift.TaskID, Kind: "monitor.drift_reconciled", At: now, Payload: core.JSONPayload(map[string]any{
			"drift_id": drift.ID, "outcome": drift.Outcome, "resolved_at": drift.ResolvedAt, "requirement_id": drift.RequirementID,
		})}); err != nil {
			return err
		}
		if drift.SystemDesignID != "" {
			return insertWorkspaceEvent(ctx, q, core.Event{Kind: "system_design.drift_resolved", At: now, Payload: core.JSONPayload(map[string]any{
				"workspace_id": workspace(ctx), "document_id": drift.SystemDesignID, "drift_id": drift.ID, "outcome": drift.Outcome, "resolved_at": drift.ResolvedAt,
			})})
		}
		return nil
	})
	return drift, err
}

func nullableMonitorEventID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) MonitorStatus(ctx context.Context, enabled bool, now time.Time) (monitor.Status, error) {
	status := monitor.Status{WorkspaceID: workspace(ctx), Enabled: enabled}
	var lastSuccess, backoff *time.Time
	err := s.pool.QueryRow(ctx, `
SELECT last_successful_at,current_error,forge_error_category,backoff_until
FROM monitor_status WHERE workspace_id=$1`, workspace(ctx)).
		Scan(&lastSuccess, &status.CurrentError, &status.ForgeErrorCategory, &backoff)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return monitor.Status{}, err
	}
	if lastSuccess != nil {
		status.LastSuccessfulAt = *lastSuccess
	}
	if backoff != nil {
		status.BackoffUntil = *backoff
	}
	activityRows, err := s.pool.Query(ctx, `
SELECT id,kind,payload_json,at FROM monitor_activity
WHERE workspace_id=$1 ORDER BY at,id`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	for activityRows.Next() {
		var activity monitor.Activity
		var payload []byte
		activity.WorkspaceID = workspace(ctx)
		if err = activityRows.Scan(&activity.ID, &activity.Kind, &payload, &activity.At); err != nil {
			activityRows.Close()
			return monitor.Status{}, err
		}
		_ = json.Unmarshal(payload, &activity.Payload)
		status.Activity = append(status.Activity, activity)
	}
	if err = activityRows.Err(); err != nil {
		activityRows.Close()
		return monitor.Status{}, err
	}
	activityRows.Close()
	rows, err := s.pool.Query(ctx, `
SELECT identity FROM monitor_observations WHERE workspace_id=$1 ORDER BY created_at`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	for rows.Next() {
		var identity string
		if err = rows.Scan(&identity); err != nil {
			rows.Close()
			return monitor.Status{}, err
		}
		record, getErr := s.getObservation(ctx, identity)
		if getErr != nil {
			rows.Close()
			return monitor.Status{}, getErr
		}
		status.Observations = append(status.Observations, record)
	}
	rows.Close()
	driftRows, err := s.pool.Query(ctx, `
SELECT id FROM repository_drift WHERE workspace_id=$1 AND resolved_at IS NULL ORDER BY detected_at`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	defer driftRows.Close()
	for driftRows.Next() {
		var id string
		if err = driftRows.Scan(&id); err != nil {
			return monitor.Status{}, err
		}
		drift, getErr := s.getDrift(ctx, id)
		if getErr != nil {
			return monitor.Status{}, getErr
		}
		status.Drift = append(status.Drift, drift)
		status.DriftCount++
		if age := now.Sub(drift.DetectedAt); age > status.OldestDriftAge {
			status.OldestDriftAge = age
		}
	}
	return status, driftRows.Err()
}

func (s *Store) RecordMonitorSuccess(ctx context.Context, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO monitor_status (workspace_id,last_successful_at) VALUES ($1,$2)
ON CONFLICT (workspace_id) DO UPDATE SET last_successful_at=$2,current_error='',forge_error_category='',backoff_until=NULL`,
		workspace(ctx), at)
	return err
}

func (s *Store) RecordMonitorFailure(ctx context.Context, category, detail string, backoffUntil time.Time) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO monitor_status (workspace_id,current_error,forge_error_category,backoff_until)
VALUES ($1,$2,$3,$4)
ON CONFLICT (workspace_id) DO UPDATE SET current_error=$2,forge_error_category=$3,backoff_until=$4`,
		workspace(ctx), detail, category, backoffUntil)
	return err
}
