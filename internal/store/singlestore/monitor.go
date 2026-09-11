package singlestore

// DEC-38: this aggregate follows component-monitor-drift and component-persistence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) WithMonitorSignalClassLock(ctx context.Context, repository string, kind monitor.SignalKind, fn func(context.Context) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return translateBackendConflict(err)
	}
	key := fmt.Sprintf("monitor-signal:%d:%s:%d:%s:%s", len(ws), ws, len(repository), repository, kind)
	release, err := s.sessionLock(ctx, key)
	if err != nil {
		return translateBackendConflict(err)
	}
	defer release()
	return fn(ctx)
}
func (s *Store) monitorTx(ctx context.Context, fn func(*sql.Tx, string) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return translateBackendConflict(err)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "monitor-records:"+ws); err != nil {
			return translateBackendConflict(err)
		}
		if err := identityWorkspace(ctx, tx, ws); err != nil {
			return translateBackendConflict(err)
		}
		return fn(tx, ws)
	})
}
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullableNumber(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func jsonStrings(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	b, _ := json.Marshal(s)
	return b
}
func validSignal(kind monitor.SignalKind) bool {
	return kind == monitor.DirectPush || kind == monitor.ExternalPRMerge || kind == monitor.PostMergeFailure || kind == monitor.LineagedMerge || kind == monitor.Revert
}

const observationColumns = "repository,kind,occurrence_id,source_url,commit_sha,pull_request_number,check_run_id,COALESCE(requirement_id,''),changed_paths,COALESCE(causal_event_id,0),observed_at,context_json,hint_context_json,COALESCE(task_id,''),task_outcome,state,deduplicated_count,forge_error_category,last_error,created_at,updated_at"

func scanObservation(row interface{ Scan(...any) error }, ws string) (monitor.ObservationRecord, error) {
	var r monitor.ObservationRecord
	var paths, ctx, hints []byte
	err := row.Scan(&r.Repository, &r.Kind, &r.OccurrenceID, &r.SourceURL, &r.CommitSHA, &r.PullRequestNumber, &r.CheckRunID, &r.RequirementID, &paths, &r.CausalEventID, &r.ObservedAt, &ctx, &hints, &r.TaskID, &r.TaskOutcome, &r.State, &r.DeduplicatedCount, &r.ForgeErrorCategory, &r.LastError, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, identityNotFound(err)
	}
	r.WorkspaceID = ws
	if err = json.Unmarshal(paths, &r.ChangedPaths); err != nil {
		return r, translateBackendConflict(err)
	}
	if err = json.Unmarshal(ctx, &r.Context); err != nil {
		return r, translateBackendConflict(err)
	}
	if len(hints) > 0 {
		if err = json.Unmarshal(hints, &r.Hints); err != nil {
			return r, translateBackendConflict(err)
		}
	}
	return r, nil
}
func (s *Store) Observe(ctx context.Context, o monitor.Observation) (monitor.ObservationRecord, bool, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return monitor.ObservationRecord{}, false, translateBackendConflict(err)
	}
	if o.WorkspaceID != "" && o.WorkspaceID != ws {
		return monitor.ObservationRecord{}, false, errors.New("monitor workspace differs from context")
	}
	if !validSignal(o.Kind) {
		return monitor.ObservationRecord{}, false, errors.New("invalid monitor signal kind")
	}
	data, err := json.Marshal(o.Context)
	if err != nil {
		return monitor.ObservationRecord{}, false, translateBackendConflict(err)
	}
	var hints any
	if o.Hints != nil {
		b, e := json.Marshal(o.Hints)
		if e != nil {
			return monitor.ObservationRecord{}, false, translateBackendConflict(e)
		}
		hints = b
	}
	fresh := false
	var r monitor.ObservationRecord
	err = s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		var e error
		r, e = scanObservation(tx.QueryRowContext(ctx, "SELECT "+observationColumns+" FROM monitor_observations WHERE workspace_id=? AND identity=?", ws, o.Identity()), ws)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return translateBackendConflict(e)
		}
		if errors.Is(e, store.ErrNotFound) {
			if o.RequirementID != "" {
				var id string
				if err := tx.QueryRowContext(ctx, "SELECT id FROM requirements WHERE workspace_id=? AND id=?", ws, o.RequirementID).Scan(&id); err != nil {
					return identityNotFound(err)
				}
			}
			_, e = writeRow(ctx, tx, rowWrite{table: "monitor_observations", operation: "INSERT", values: map[string]any{"workspace_id": ws, "identity": o.Identity(), "repository": o.Repository, "kind": string(o.Kind), "occurrence_id": o.OccurrenceID, "source_url": o.SourceURL, "commit_sha": o.CommitSHA, "pull_request_number": o.PullRequestNumber, "check_run_id": o.CheckRunID, "requirement_id": nullableText(o.RequirementID), "changed_paths": jsonStrings(o.ChangedPaths), "causal_event_id": nullableNumber(o.CausalEventID), "observed_at": o.ObservedAt, "context_json": data, "hint_context_json": hints, "created_at": o.ObservedAt, "updated_at": o.ObservedAt}})
			fresh = e == nil
		} else {
			if len(o.ChangedPaths) == 0 {
				o.ChangedPaths = r.ChangedPaths
			}
			if o.CausalEventID == 0 {
				o.CausalEventID = r.CausalEventID
			}
			_, e = tx.ExecContext(ctx, "UPDATE monitor_observations SET deduplicated_count=deduplicated_count+1,changed_paths=?,causal_event_id=?,state='deduplicated',updated_at=? WHERE workspace_id=? AND identity=?", jsonStrings(o.ChangedPaths), nullableNumber(o.CausalEventID), o.ObservedAt, ws, o.Identity())
		}
		if e != nil {
			return translateBackendConflict(e)
		}
		r, e = scanObservation(tx.QueryRowContext(ctx, "SELECT "+observationColumns+" FROM monitor_observations WHERE workspace_id=? AND identity=?", ws, o.Identity()), ws)
		return translateBackendConflict(e)
	})
	return r, fresh && err == nil, translateBackendConflict(err)
}
func (s *Store) FindOpenMonitorTask(ctx context.Context, repository string, kind monitor.SignalKind) (string, bool, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return "", false, translateBackendConflict(err)
	}
	var id string
	err = s.db.QueryRowContext(ctx, "SELECT id FROM tasks WHERE workspace_id=? AND repo_name=? AND source=? AND state NOT IN ('merged','closed') ORDER BY created_at,id LIMIT 1", ws, repository, "monitor:"+string(kind)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, translateBackendConflict(err)
}
func lockMonitorTask(ctx context.Context, tx *sql.Tx, ws, taskID string) error {
	var id string
	return identityNotFound(tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE", ws, taskID).Scan(&id))
}
func checkDriftSaturation(taskID string, count int) error {
	if count >= monitor.MaxUnresolvedDriftPerTask {
		return monitor.TaskDriftSaturatedError(taskID)
	}
	return nil
}
func monitorTaskCapacity(ctx context.Context, tx *sql.Tx, ws, taskID string) error {
	if err := lockMonitorTask(ctx, tx, ws, taskID); err != nil {
		return translateBackendConflict(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repository_drift WHERE workspace_id=? AND task_id=? AND resolved_at IS NULL", ws, taskID).Scan(&count); err != nil {
		return translateBackendConflict(err)
	}
	return checkDriftSaturation(taskID, count)
}
func (s *Store) LinkTask(ctx context.Context, identity, taskID, outcome string) (monitor.ObservationRecord, error) {
	var r monitor.ObservationRecord
	err := s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		var err error
		r, err = scanObservation(tx.QueryRowContext(ctx, "SELECT "+observationColumns+" FROM monitor_observations WHERE workspace_id=? AND identity=?", ws, identity), ws)
		if err != nil {
			return translateBackendConflict(err)
		}
		if r.TaskID != "" {
			if r.TaskID != taskID {
				return fmt.Errorf("monitor observation %s already links task %s", identity, r.TaskID)
			}
			return nil
		}
		if r.Kind.Drift() {
			if err := monitorTaskCapacity(ctx, tx, ws, taskID); err != nil {
				return translateBackendConflict(err)
			}
		} else if err := lockMonitorTask(ctx, tx, ws, taskID); err != nil {
			return translateBackendConflict(err)
		}
		r.TaskID = taskID
		r.TaskOutcome = outcome
		r.State = "task_linked"
		r.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		_, err = tx.ExecContext(ctx, "UPDATE monitor_observations SET task_id=?,task_outcome=?,state='task_linked',updated_at=? WHERE workspace_id=? AND identity=?", taskID, outcome, r.UpdatedAt, ws, identity)
		return translateBackendConflict(err)
	})
	return r, translateBackendConflict(err)
}

const driftColumns = "id,repository,kind,source_url,commit_sha,COALESCE(requirement_id,''),COALESCE(system_design_id,''),COALESCE(system_design_version,0),COALESCE(causal_event_id,0),matching_paths,task_id,detected_at,resolved_at,outcome"

func scanDrift(row interface{ Scan(...any) error }, ws string) (monitor.Drift, error) {
	var d monitor.Drift
	var paths []byte
	var resolved sql.NullTime
	err := row.Scan(&d.ID, &d.Repository, &d.Kind, &d.SourceURL, &d.CommitSHA, &d.RequirementID, &d.SystemDesignID, &d.SystemDesignVersion, &d.CausalEventID, &paths, &d.TaskID, &d.DetectedAt, &resolved, &d.Outcome)
	if err != nil {
		return d, identityNotFound(err)
	}
	d.WorkspaceID = ws
	d.ResolvedAt = resolved.Time
	err = json.Unmarshal(paths, &d.MatchingPaths)
	return d, translateBackendConflict(err)
}
func (s *Store) RecordDrift(ctx context.Context, d monitor.Drift) (monitor.Drift, bool, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return monitor.Drift{}, false, translateBackendConflict(err)
	}
	if d.WorkspaceID != "" && d.WorkspaceID != ws {
		return monitor.Drift{}, false, errors.New("monitor workspace differs from context")
	}
	if !d.Kind.Drift() {
		return monitor.Drift{}, false, errors.New("invalid drift kind")
	}
	fresh := false
	var r monitor.Drift
	err = s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		var e error
		r, e = scanDrift(tx.QueryRowContext(ctx, "SELECT "+driftColumns+" FROM repository_drift WHERE workspace_id=? AND id=?", ws, d.ID), ws)
		if e == nil {
			return nil
		}
		if !errors.Is(e, store.ErrNotFound) {
			return translateBackendConflict(e)
		}
		if e = monitorTaskCapacity(ctx, tx, ws, d.TaskID); e != nil {
			return translateBackendConflict(e)
		}
		if d.RequirementID != "" {
			var id string
			if e = tx.QueryRowContext(ctx, "SELECT id FROM requirements WHERE workspace_id=? AND id=?", ws, d.RequirementID).Scan(&id); e != nil {
				return identityNotFound(e)
			}
		}
		if d.SystemDesignID != "" {
			var id string
			if e = tx.QueryRowContext(ctx, "SELECT id FROM system_designs WHERE workspace_id=? AND id=?", ws, d.SystemDesignID).Scan(&id); e != nil {
				return identityNotFound(e)
			}
		}
		_, e = writeRow(ctx, tx, rowWrite{table: "repository_drift", operation: "INSERT", values: map[string]any{"workspace_id": ws, "id": d.ID, "repository": d.Repository, "kind": string(d.Kind), "source_url": d.SourceURL, "commit_sha": d.CommitSHA, "requirement_id": nullableText(d.RequirementID), "system_design_id": nullableText(d.SystemDesignID), "system_design_version": nullableNumber(int64(d.SystemDesignVersion)), "causal_event_id": nullableNumber(d.CausalEventID), "matching_paths": jsonStrings(d.MatchingPaths), "task_id": d.TaskID, "detected_at": d.DetectedAt}})
		if e != nil {
			return translateBackendConflict(e)
		}
		fresh = true
		if d.SystemDesignID != "" {
			if _, e = appendWorkspaceEvent(ctx, tx, ws, "system_design.drift_detected", map[string]any{"workspace_id": ws, "document_id": d.SystemDesignID, "version": d.SystemDesignVersion, "drift_id": d.ID, "causal_event_id": d.CausalEventID, "matching_paths": d.MatchingPaths}); e != nil {
				return translateBackendConflict(e)
			}
		}
		r, e = scanDrift(tx.QueryRowContext(ctx, "SELECT "+driftColumns+" FROM repository_drift WHERE workspace_id=? AND id=?", ws, d.ID), ws)
		return translateBackendConflict(e)
	})
	return r, fresh && err == nil, translateBackendConflict(err)
}
func (s *Store) AuditMonitor(ctx context.Context, kind string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return translateBackendConflict(err)
	}
	return s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		_, err := writeRow(ctx, tx, rowWrite{table: "monitor_activity", operation: "INSERT", values: map[string]any{"workspace_id": ws, "kind": kind, "payload_json": data}})
		return translateBackendConflict(err)
	})
}
func auditMonitorTask(ctx context.Context, tx *sql.Tx, ws, taskID, kind string, payload map[string]any) error {
	if err := lockMonitorTask(ctx, tx, ws, taskID); err != nil {
		return translateBackendConflict(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return translateBackendConflict(err)
	}
	actor := store.ActorFromContext(ctx)
	_, err = writeRow(ctx, tx, rowWrite{table: "events", operation: "INSERT", values: map[string]any{"workspace_id": ws, "task_id": taskID, "kind": kind, "payload_json": data, "actor_id": actor.ID, "actor_role": string(actor.Role), "at": time.Now().UTC()}})
	return translateBackendConflict(err)
}
func (s *Store) AuditTask(ctx context.Context, taskID, kind string, payload map[string]any) error {
	return s.monitorTx(ctx, func(tx *sql.Tx, ws string) error { return auditMonitorTask(ctx, tx, ws, taskID, kind, payload) })
}
func (s *Store) RecordMonitorSuccess(ctx context.Context, at time.Time) error {
	return s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO monitor_status(workspace_id,last_successful_at) VALUES(?,?) ON DUPLICATE KEY UPDATE last_successful_at=VALUES(last_successful_at),current_error='',forge_error_category='',backoff_until=NULL", ws, at)
		return translateBackendConflict(err)
	})
}
func (s *Store) RecordMonitorFailure(ctx context.Context, category, detail string, backoff time.Time) error {
	return s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO monitor_status(workspace_id,current_error,forge_error_category,backoff_until) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE current_error=VALUES(current_error),forge_error_category=VALUES(forge_error_category),backoff_until=VALUES(backoff_until)", ws, detail, category, backoff)
		return translateBackendConflict(err)
	})
}
func (s *Store) MonitorStatus(ctx context.Context, enabled bool, now time.Time) (monitor.Status, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return monitor.Status{}, translateBackendConflict(err)
	}
	r := monitor.Status{WorkspaceID: ws, Enabled: enabled}
	var last, backoff sql.NullTime
	err = s.db.QueryRowContext(ctx, "SELECT last_successful_at,current_error,forge_error_category,backoff_until FROM monitor_status WHERE workspace_id=?", ws).Scan(&last, &r.CurrentError, &r.ForgeErrorCategory, &backoff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, translateBackendConflict(err)
	}
	r.LastSuccessfulAt = last.Time
	r.BackoffUntil = backoff.Time
	rows, err := s.db.QueryContext(ctx, "SELECT id,kind,payload_json,at FROM monitor_activity WHERE workspace_id=? ORDER BY at,id", ws)
	if err != nil {
		return r, translateBackendConflict(err)
	}
	for rows.Next() {
		a := monitor.Activity{WorkspaceID: ws}
		var data []byte
		if err = rows.Scan(&a.ID, &a.Kind, &data, &a.At); err != nil {
			rows.Close()
			return r, translateBackendConflict(err)
		}
		if err = json.Unmarshal(data, &a.Payload); err != nil {
			rows.Close()
			return r, translateBackendConflict(err)
		}
		r.Activity = append(r.Activity, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, translateBackendConflict(err)
	}
	rows, err = s.db.QueryContext(ctx, "SELECT "+observationColumns+" FROM monitor_observations WHERE workspace_id=? ORDER BY created_at,identity", ws)
	if err != nil {
		return r, translateBackendConflict(err)
	}
	for rows.Next() {
		o, e := scanObservation(rows, ws)
		if e != nil {
			rows.Close()
			return r, translateBackendConflict(e)
		}
		r.Observations = append(r.Observations, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, translateBackendConflict(err)
	}
	r.Drift, err = s.ListUnresolvedDrift(ctx)
	if err != nil {
		return r, err
	}
	r.DriftCount = len(r.Drift)
	for _, d := range r.Drift {
		if age := now.Sub(d.DetectedAt); age > r.OldestDriftAge {
			r.OldestDriftAge = age
		}
	}
	return r, nil
}

func (s *Store) ListUnresolvedDrift(ctx context.Context) ([]monitor.Drift, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+driftColumns+" FROM repository_drift WHERE workspace_id=? AND resolved_at IS NULL ORDER BY detected_at,id", ws)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []monitor.Drift
	for rows.Next() {
		d, err := scanDrift(rows, ws)
		if err != nil {
			return nil, translateBackendConflict(err)
		}
		result = append(result, d)
	}
	return result, translateBackendConflict(rows.Err())
}

func (s *Store) ResolveDrift(ctx context.Context, id, outcome, requirementID string) (monitor.Drift, error) {
	requirementID = strings.TrimSpace(requirementID)
	if outcome != "requirements_amended" && outcome != "design_document_updated" && outcome != "conflict_resolved" && outcome != "change_reverted" {
		return monitor.Drift{}, fmt.Errorf("unsupported audited reconciliation outcome %q", outcome)
	}
	if outcome != "requirements_amended" && requirementID != "" {
		return monitor.Drift{}, monitor.ErrRequirementIDNotAllowed
	}
	var d monitor.Drift
	err := s.monitorTx(ctx, func(tx *sql.Tx, ws string) error {
		var err error
		d, err = scanDrift(tx.QueryRowContext(ctx, "SELECT "+driftColumns+" FROM repository_drift WHERE workspace_id=? AND id=? FOR UPDATE", ws, id), ws)
		if err != nil {
			return translateBackendConflict(err)
		}
		if !d.ResolvedAt.IsZero() {
			if requirementID != "" && requirementID != d.RequirementID {
				return monitor.ErrRequirementIDInvalid
			}
			return nil
		}
		if outcome == "requirements_amended" {
			if requirementID != "" && d.RequirementID != "" && requirementID != d.RequirementID {
				return monitor.ErrRequirementIDInvalid
			}
			if d.RequirementID == "" {
				d.RequirementID = requirementID
			}
			if d.RequirementID == "" {
				return monitor.ErrRequirementIDMissing
			}
			if err = proposeDriftRequirement(ctx, tx, ws, d); err != nil {
				return translateBackendConflict(err)
			}
		}
		if outcome == "design_document_updated" {
			if d.SystemDesignID == "" {
				return errors.New("design_document_updated requires a system design")
			}
			var current sql.NullInt64
			if err = tx.QueryRowContext(ctx, "SELECT current_version FROM system_designs WHERE workspace_id=? AND id=? FOR UPDATE", ws, d.SystemDesignID).Scan(&current); err != nil {
				return translateBackendConflict(err)
			}
			if !current.Valid || current.Int64 <= int64(d.SystemDesignVersion) {
				return errors.New("drift requires a confirmed replacement design version")
			}
			var confirmed bool
			if err = tx.QueryRowContext(ctx, "SELECT confirmed FROM system_design_versions WHERE workspace_id=? AND document_id=? AND version=?", ws, d.SystemDesignID, current.Int64).Scan(&confirmed); err != nil {
				return translateBackendConflict(err)
			}
			if !confirmed {
				return errors.New("drift requires a confirmed replacement design version")
			}
		}
		d.ResolvedAt = time.Now().UTC().Truncate(time.Microsecond)
		d.Outcome = outcome
		if _, err = tx.ExecContext(ctx, "UPDATE repository_drift SET requirement_id=?,resolved_at=?,outcome=? WHERE workspace_id=? AND id=?", nullableText(d.RequirementID), d.ResolvedAt, outcome, ws, id); err != nil {
			return translateBackendConflict(err)
		}
		if err = auditMonitorTask(ctx, tx, ws, d.TaskID, "monitor.drift_reconciled", map[string]any{"drift_id": d.ID, "outcome": outcome, "resolved_at": d.ResolvedAt, "requirement_id": d.RequirementID}); err != nil {
			return translateBackendConflict(err)
		}
		if d.SystemDesignID != "" {
			_, err = appendWorkspaceEvent(ctx, tx, ws, "system_design.drift_resolved", map[string]any{"workspace_id": ws, "document_id": d.SystemDesignID, "drift_id": d.ID, "outcome": outcome, "resolved_at": d.ResolvedAt})
		}
		return translateBackendConflict(err)
	})
	return d, translateBackendConflict(err)
}

// Drift resolution owns this proposal transaction; confirmation remains an operator act.
func proposeDriftRequirement(ctx context.Context, tx *sql.Tx, ws string, d monitor.Drift) error {
	var currentVersion sql.NullInt64
	var high int
	err := tx.QueryRowContext(ctx, "SELECT current_version,statement_high_water_mark FROM requirements WHERE workspace_id=? AND id=? FOR UPDATE", ws, d.RequirementID).Scan(&currentVersion, &high)
	if errors.Is(err, sql.ErrNoRows) {
		return monitor.ErrUnknownRequirementID
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	if !currentVersion.Valid {
		return monitor.ErrRequirementIDInvalid
	}
	var current core.RequirementVersion
	var data []byte
	err = tx.QueryRowContext(ctx, "SELECT content,statements_json,confirmed FROM requirement_versions WHERE workspace_id=? AND requirement_id=? AND version=?", ws, d.RequirementID, currentVersion.Int64).Scan(&current.Content, &data, &current.Confirmed)
	if err != nil {
		return translateBackendConflict(err)
	}
	if !current.Confirmed {
		return monitor.ErrRequirementIDInvalid
	}
	current.RequirementID = d.RequirementID
	current.Version = int(currentVersion.Int64)
	current.Workspace = ws
	if err = json.Unmarshal(data, &current.Statements); err != nil {
		return translateBackendConflict(err)
	}
	proposal, err := store.DriftAmendmentVersion(d, current)
	if err != nil {
		return translateBackendConflict(err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT version,statements_json FROM requirement_versions WHERE workspace_id=? AND requirement_id=?", ws, d.RequirementID)
	if err != nil {
		return translateBackendConflict(err)
	}
	latest := 0
	var issued []string
	for rows.Next() {
		var n int
		var raw []byte
		if err = rows.Scan(&n, &raw); err != nil {
			rows.Close()
			return translateBackendConflict(err)
		}
		if n > latest {
			latest = n
		}
		var statements []core.RequirementStatement
		if err = json.Unmarshal(raw, &statements); err != nil {
			rows.Close()
			return translateBackendConflict(err)
		}
		for _, statement := range statements {
			issued = append(issued, statement.ID)
			for _, ac := range statement.AcceptanceCriteria {
				issued = append(issued, ac.ID)
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return translateBackendConflict(err)
	}
	if err = core.ValidateRequirementRevision(high, issued, proposal.Statements); err != nil {
		return translateBackendConflict(err)
	}
	data, err = json.Marshal(proposal.Statements)
	if err != nil {
		return translateBackendConflict(err)
	}
	now := time.Now().UTC()
	proposal.Version = latest + 1
	if _, err = writeRow(ctx, tx, rowWrite{table: "requirement_versions", operation: "INSERT", values: map[string]any{"workspace_id": ws, "requirement_id": d.RequirementID, "version": proposal.Version, "content": proposal.Content, "statements_json": data, "origin": string(proposal.Origin), "origin_drift_id": d.ID, "confirmed": false, "created_at": now}}); err != nil {
		return translateBackendConflict(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE requirements SET updated_at=? WHERE workspace_id=? AND id=?", now, ws, d.RequirementID); err != nil {
		return translateBackendConflict(err)
	}
	_, err = appendWorkspaceEvent(ctx, tx, ws, "requirement.version_proposed", map[string]any{"workspace_id": ws, "requirement_id": d.RequirementID, "version": proposal.Version, "origin": proposal.Origin, "origin_drift_id": d.ID, "statement_count": len(proposal.Statements)})
	return translateBackendConflict(err)
}
