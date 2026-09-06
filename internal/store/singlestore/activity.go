package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) CreateFeature(ctx context.Context, f core.Feature) error {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		ws, err := workspace(ctx)
		if err != nil {
			return err
		}
		if err = workerWorkspaceExists(ctx, tx, ws); err != nil {
			return err
		}
		if f.ParentID != "" {
			var found int
			if err = tx.QueryRowContext(ctx, `SELECT 1 FROM features WHERE workspace_id=? AND id=?`, ws, f.ParentID).Scan(&found); err != nil {
				return notFound(err, "parent feature %s", f.ParentID)
			}
		}
		if err = lockKey(ctx, tx, "feature-id:"+f.ID); err != nil {
			return err
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM features WHERE id=?)`, f.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("feature %s already exists", f.ID)
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO features(id,workspace_id,parent_id,name,description,created_at) VALUES(?,?,?,?,?,?)`, f.ID, ws, nullString(f.ParentID), f.Name, f.Description, f.CreatedAt)
		return err
	})
}
func (s *Store) ListFeatures(ctx context.Context) ([]core.Feature, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,workspace_id,COALESCE(parent_id,''),name,description,created_at FROM features WHERE workspace_id=? ORDER BY name,id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Feature
	for rows.Next() {
		var f core.Feature
		if err = rows.Scan(&f.ID, &f.Workspace, &f.ParentID, &f.Name, &f.Description, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (s *Store) AssignTaskFeature(ctx context.Context, id, feature string) error {
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		if _, err := getTaskRow(ctx, tx, id); err != nil {
			return err
		}
		if feature != "" {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM features WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), feature).Scan(&found); err != nil {
				return notFound(err, "feature %s", feature)
			}
		}
		if err := taskWrite(ctx, tx, id, map[string]any{"feature_id": nullString(feature)}); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": feature})})
	})
}
func (s *Store) UpsertTranscript(ctx context.Context, t core.Transcript) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	var task string
	if err := documentRow(ctx, s.db, `SELECT task_id FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), t.JobID).Scan(&task); err != nil {
		return notFound(err, "job %s", t.JobID)
	}
	return s.taskTx(ctx, task, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transcripts(workspace_id,job_id,uri,redaction_stats,created_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE uri=VALUES(uri),redaction_stats=VALUES(redaction_stats),created_at=VALUES(created_at)`, documentWorkspace(ctx), t.JobID, t.URI, core.JSONPayload(t.RedactionStats), t.CreatedAt); err != nil {
			return err
		}
		if id := strings.TrimPrefix(t.URI, "artifact://"); id != t.URI {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_links WHERE workspace_id=? AND artifact_id=? AND task_id=? AND role=?)`, documentWorkspace(ctx), id, task, core.ArtifactRoleGeneratedAudit).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, `UPDATE artifact_links SET role=? WHERE workspace_id=? AND artifact_id=? AND task_id=? AND role=?`, core.ArtifactRoleGeneratedAudit, documentWorkspace(ctx), id, task, core.ArtifactRoleTaskContext); err != nil {
					return err
				}
			}
		}
		return taskEvent(ctx, tx, core.Event{TaskID: task, JobID: t.JobID, Kind: "transcript.persisted", Payload: core.JSONPayload(map[string]any{"uri": t.URI, "redaction_stats": t.RedactionStats})})
	})
}
func (s *Store) GetTranscript(ctx context.Context, id string) (core.Transcript, error) {
	var t core.Transcript
	var stats []byte
	err := documentRow(ctx, s.db, `SELECT job_id,uri,redaction_stats,created_at FROM transcripts WHERE workspace_id=? AND job_id=?`, documentWorkspace(ctx), id).Scan(&t.JobID, &t.URI, &stats, &t.CreatedAt)
	if err != nil {
		return core.Transcript{}, notFound(err, "transcript for job %s", id)
	}
	err = json.Unmarshal(stats, &t.RedactionStats)
	return t, err
}
func prepareArtifact(ctx context.Context, a core.Artifact, content []byte) (core.Artifact, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return core.Artifact{}, err
	}
	a.Workspace = ws
	if a.Role == "" {
		a.Role = core.ArtifactRoleTaskContext
	}
	if !a.Role.Valid() {
		return core.Artifact{}, fmt.Errorf("invalid artifact role %q", a.Role)
	}
	a.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	a.SizeBytes = int64(len(content))
	if err = a.ValidateAttachmentTarget(); err != nil {
		return core.Artifact{}, err
	}
	if a.Role == core.ArtifactRoleVerificationEvidence {
		if a.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("verification evidence must be attached directly to one task")
		}
		a.ContentType, err = core.NormalizeVerificationEvidenceContentType(a.ContentType, a.SizeBytes)
		if err != nil {
			return core.Artifact{}, err
		}
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	return a, nil
}
func insertArtifactTx(ctx context.Context, tx *sql.Tx, a core.Artifact, content []byte) error {
	if err := workerWorkspaceExists(ctx, tx, a.Workspace); err != nil {
		return err
	}
	if err := lockKey(ctx, tx, "artifact:"+a.Workspace+":"+a.ID); err != nil {
		return err
	}
	for _, p := range []struct{ table, id string }{{"tasks", a.TaskID}, {"features", a.FeatureID}, {"requirements", a.RequirementID}, {"planning_sessions", a.PlanningSessionID}} {
		if p.id == "" {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+p.table+` WHERE workspace_id=? AND id=?`, a.Workspace, p.id).Scan(&exists); err != nil {
			return notFound(err, "artifact attachment %s", p.id)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(id,workspace_id,name,content_type,size_bytes,content,created_at) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=VALUES(id)`, a.ID, a.Workspace, a.Name, a.ContentType, a.SizeBytes, content, a.CreatedAt); err != nil {
		return err
	}
	if a.TaskID == "" && a.FeatureID == "" && a.RequirementID == "" && a.PlanningSessionID == "" {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_links WHERE workspace_id=? AND artifact_id=? AND COALESCE(task_id,'')=? AND COALESCE(feature_id,'')=? AND COALESCE(requirement_id,'')=? AND COALESCE(planning_session_id,'')=? AND role=?)`, a.Workspace, a.ID, a.TaskID, a.FeatureID, a.RequirementID, a.PlanningSessionID, a.Role).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO artifact_links(workspace_id,artifact_id,task_id,feature_id,requirement_id,planning_session_id,role) VALUES(?,?,?,?,?,?,?)`, a.Workspace, a.ID, nullString(a.TaskID), nullString(a.FeatureID), nullString(a.RequirementID), nullString(a.PlanningSessionID), a.Role)
	return err
}
func (s *Store) CreateArtifact(ctx context.Context, a core.Artifact, content []byte) (core.Artifact, error) {
	a, err := prepareArtifact(ctx, a, content)
	if err != nil {
		return core.Artifact{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error { return insertArtifactTx(ctx, tx, a, content) })
	if err != nil {
		return core.Artifact{}, err
	}
	return a, nil
}
func (s *Store) CreateClaimedVerificationEvidence(ctx context.Context, r store.ClaimedVerificationEvidenceRequest, content []byte) (core.Artifact, error) {
	if strings.TrimSpace(r.WorkOrderID) == "" || strings.TrimSpace(r.WorkerID) == "" || strings.TrimSpace(r.SessionID) == "" || strings.TrimSpace(r.ClientToken) == "" {
		return core.Artifact{}, store.ErrVerificationEvidenceClaimConflict
	}
	var a core.Artifact
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(r.ClientToken)))
	err := s.orderTx(ctx, r.WorkOrderID, func(tx *sql.Tx, o core.WorkOrder) error {
		now := time.Now().UTC()
		if o.Stage != core.StageImplement || o.State != core.WorkOrderClaimed || o.WorkerID != r.WorkerID || o.SessionID != r.SessionID || o.ClientTokenHash != hash || !o.LeaseExpiresAt.After(now) || (!o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now)) {
			return store.ErrVerificationEvidenceClaimConflict
		}
		var err error
		a, err = prepareArtifact(ctx, core.Artifact{TaskID: o.TaskID, Name: r.Name, ContentType: r.ContentType, Role: core.ArtifactRoleVerificationEvidence}, content)
		if err != nil {
			return err
		}
		return insertArtifactTx(ctx, tx, a, content)
	})
	if errors.Is(err, store.ErrNotFound) {
		err = store.ErrVerificationEvidenceClaimConflict
	}
	if err != nil {
		return core.Artifact{}, err
	}
	return a, nil
}

func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := documentRow(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.feature_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=? AND a.id=? ORDER BY l.role LIMIT 1`, documentWorkspace(ctx), id).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.FeatureID, &artifact.RequirementID, &artifact.PlanningSessionID)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) GetArtifactForPlanningSession(ctx context.Context, id, sessionID string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := documentRow(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,l.role,COALESCE(l.task_id,''),COALESCE(l.feature_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'')
		FROM artifacts a
		JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		WHERE a.workspace_id=? AND a.id=? AND l.planning_session_id=?
		ORDER BY l.role LIMIT 1`, documentWorkspace(ctx), id, sessionID).Scan(
		&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType,
		&artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.Role,
		&artifact.TaskID, &artifact.FeatureID, &artifact.RequirementID, &artifact.PlanningSessionID,
	)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	rows, err := documentRows(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.feature_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=? ORDER BY a.created_at,a.id,l.role`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.FeatureID, &artifact.RequirementID, &artifact.PlanningSessionID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func checkpointHistory(events []core.Event, id, worker string, c core.WorkOrderAttemptCheckpoint) (bool, bool) {
	claimed, checkpointed := false, false
	for _, e := range events {
		if e.Kind == "work_order.claimed" {
			var o core.WorkOrder
			if json.Unmarshal(e.Payload, &o) == nil && o.ID == id && o.WorkerID == worker && o.SessionID == c.SessionID && o.AttemptID == c.AttemptID {
				claimed = true
			}
		}
		if e.Kind == "work_order.attempt_checkpointed" {
			var p struct {
				ID      string `json:"work_order_id"`
				Attempt string `json:"attempt_id"`
				Commit  string `json:"commit_sha"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.ID == id && p.Attempt == c.AttemptID && p.Commit == c.CommitSHA {
				checkpointed = true
			}
		}
	}
	return claimed, checkpointed
}
func (s *Store) RecordWorkOrderAttemptCheckpoint(ctx context.Context, id, worker string, c core.WorkOrderAttemptCheckpoint) (bool, error) {
	recorded := false
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		events, err := documentTaskEvents(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		claimed, exists := checkpointHistory(events, id, worker, c)
		if !o.AuthorizesAttemptCheckpoint(worker, c, time.Now().UTC()) && !(o.Stage == core.StageImplement && claimed) {
			return store.ErrWorkOrderClaimLost
		}
		if exists {
			return nil
		}
		if err = taskEvent(workerClaimActorContext(ctx, worker), tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.attempt_checkpointed", Payload: store.AttemptCheckpointPayload(o, c), At: time.Now().UTC()}); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		err = store.ErrWorkOrderClaimLost
	}
	return recorded, err
}
func (s *Store) UpsertWorkOrderActivitySnapshot(ctx context.Context, id string, c core.WorkOrderClaimIdentity, content string) error {
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		now := time.Now().UTC()
		if !claimAlive(o, c, now) || o.AttemptID == "" {
			return store.ErrWorkOrderClaimLost
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO work_order_activity_snapshots(workspace_id,work_order_id,attempt_id,content,captured_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE attempt_id=VALUES(attempt_id),content=VALUES(content),captured_at=VALUES(captured_at)`, documentWorkspace(ctx), id, o.AttemptID, content, now)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrWorkOrderClaimLost
	}
	return err
}
func (s *Store) FinalizeWorkOrderAttemptObservability(ctx context.Context, id, worker string, c core.WorkOrderAttemptCheckpoint) error {
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		events, err := documentTaskEvents(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		claimed, checkpointed := checkpointHistory(events, id, worker, c)
		if !claimed || !checkpointed {
			return store.ErrWorkOrderClaimLost
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM work_order_activity_snapshots WHERE workspace_id=? AND work_order_id=? AND attempt_id=?`, documentWorkspace(ctx), id, c.AttemptID); err != nil {
			return err
		}
		if c.Transcript != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO work_order_transcript_captures(workspace_id,work_order_id,attempt_id,content,termination_reason,truncated,captured_at) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE attempt_id=VALUES(attempt_id)`, documentWorkspace(ctx), id, c.AttemptID, c.Transcript.Content, c.TerminationReason, c.Transcript.Truncated, time.Now().UTC())
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrWorkOrderClaimLost
	}
	return err
}

func (s *Store) GetWorkOrderActivitySnapshot(ctx context.Context, workOrderID string) (core.WorkOrderActivitySnapshot, bool, error) {
	var snapshot core.WorkOrderActivitySnapshot
	err := documentRow(ctx, s.db, `SELECT attempt_id, content, captured_at
		FROM work_order_activity_snapshots
		WHERE workspace_id=? AND work_order_id=?`, documentWorkspace(ctx), workOrderID).
		Scan(&snapshot.AttemptID, &snapshot.Content, &snapshot.CapturedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.WorkOrderActivitySnapshot{}, false, nil
		}
		return core.WorkOrderActivitySnapshot{}, false, fmt.Errorf("get work-order activity snapshot: %w", err)
	}
	return snapshot, true, nil
}

func (s *Store) ListWorkOrderTranscriptCaptures(ctx context.Context, workOrderID string) ([]core.WorkOrderTranscriptCapture, error) {
	rows, err := documentRows(ctx, s.db, `SELECT attempt_id, content, termination_reason, truncated, captured_at
		FROM work_order_transcript_captures
		WHERE workspace_id=? AND work_order_id=?
		ORDER BY captured_at, attempt_id`, documentWorkspace(ctx), workOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	captures := make([]core.WorkOrderTranscriptCapture, 0)
	for rows.Next() {
		var capture core.WorkOrderTranscriptCapture
		if err = rows.Scan(&capture.AttemptID, &capture.Content, &capture.TerminationReason, &capture.Truncated, &capture.CapturedAt); err != nil {
			return nil, err
		}
		captures = append(captures, capture)
	}
	return captures, rows.Err()
}
