package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

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
	if a.Workspace != "" && a.Workspace != ws {
		return core.Artifact{}, fmt.Errorf("artifact workspace mismatch")
	}
	a.ContentType, err = core.ValidateArtifactMedia(a.ContentType, content)
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
	if a.Role == core.ArtifactRoleTypedVerificationEvidence {
		if a.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("typed evidence requires a task")
		}
		if _, err := core.ValidateTypedVerificationArtifact(a.ContentType, content); err != nil {
			return core.Artifact{}, err
		}
	}
	if a.Role == core.ArtifactRoleVerificationEvidence {
		if a.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("verification evidence must be attached directly to one task")
		}
		a.ContentType, err = core.ValidateVerificationEvidenceArtifact(a.ContentType, content)
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
	if err := workspaceRowExists(ctx, tx, a.Workspace); err != nil {
		return err
	}
	if err := lockKey(ctx, tx, "artifact:"+a.Workspace+":"+a.ID); err != nil {
		return err
	}
	for _, p := range []struct{ table, id string }{{"tasks", a.TaskID}, {"requirements", a.RequirementID}, {"planning_sessions", a.PlanningSessionID}} {
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
	if a.TaskID == "" && a.RequirementID == "" && a.PlanningSessionID == "" {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_links WHERE workspace_id=? AND artifact_id=? AND COALESCE(task_id,'')=? AND COALESCE(requirement_id,'')=? AND COALESCE(planning_session_id,'')=? AND role=?)`, a.Workspace, a.ID, a.TaskID, a.RequirementID, a.PlanningSessionID, a.Role).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO artifact_links(workspace_id,artifact_id,task_id,requirement_id,planning_session_id,role) VALUES(?,?,?,?,?,?)`, a.Workspace, a.ID, nullString(a.TaskID), nullString(a.RequirementID), nullString(a.PlanningSessionID), a.Role)
	return err
}
func (s *Store) CreateArtifact(ctx context.Context, a core.Artifact, content []byte) (core.Artifact, error) {
	if a.Role == core.ArtifactRoleTypedVerificationEvidence {
		return core.Artifact{}, store.ErrVerificationAccess
	}
	a, err := prepareArtifact(ctx, a, content)
	if err != nil {
		return core.Artifact{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := insertArtifactTx(ctx, tx, a, content); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT name,content_type,size_bytes,created_at FROM artifacts WHERE workspace_id=? AND id=?`, a.Workspace, a.ID).Scan(&a.Name, &a.ContentType, &a.SizeBytes, &a.CreatedAt)
	})
	if err != nil {
		return core.Artifact{}, err
	}
	return a, nil
}
func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := documentRow(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=? AND a.id=? ORDER BY (l.role='typed_verification_evidence') DESC,l.role LIMIT 1`, documentWorkspace(ctx), id).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	if artifact.Role == core.ArtifactRoleTypedVerificationEvidence {
		return core.Artifact{}, nil, store.ErrVerificationAccess
	}
	return artifact, content, nil
}

func (s *Store) GetArtifactForPlanningSession(ctx context.Context, id, sessionID string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := documentRow(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,l.role,COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'')
		FROM artifacts a
		JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		WHERE a.workspace_id=? AND a.id=? AND l.planning_session_id=?
		ORDER BY l.role LIMIT 1`, documentWorkspace(ctx), id, sessionID).Scan(
		&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType,
		&artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.Role,
		&artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID,
	)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	rows, err := documentRows(ctx, s.db, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=? ORDER BY a.created_at,a.id,l.role`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

// attemptCaptureInsertFault is a test-only seam that fails the capture
// transaction after the snapshot supersession and before the insert, so the
// native integration test can prove the rollback leaves no partial capture.
// SingleStore has no triggers to inject that failure. It is nil in production.
var attemptCaptureInsertFault func() error
