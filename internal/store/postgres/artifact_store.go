package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	if artifact.Role == core.ArtifactRoleTypedVerificationEvidence {
		return core.Artifact{}, store.ErrVerificationAccess
	}
	var result core.Artifact
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		result, err = s.createArtifactTx(ctx, tx, artifact, content)
		return err
	})
	return result, err
}

func (s *Store) createArtifactTx(ctx context.Context, tx pgx.Tx, artifact core.Artifact, content []byte) (core.Artifact, error) {
	media, mediaErr := core.ValidateArtifactMedia(artifact.ContentType, content)
	if mediaErr != nil {
		return core.Artifact{}, mediaErr
	}
	artifact.ContentType = media
	if artifact.Workspace != "" && artifact.Workspace != workspace(ctx) {
		return core.Artifact{}, fmt.Errorf("artifact workspace mismatch")
	}

	if artifact.Role == "" {
		artifact.Role = core.ArtifactRoleTaskContext
	}
	if !artifact.Role.Valid() {
		return core.Artifact{}, fmt.Errorf("invalid artifact role %q", artifact.Role)
	}
	artifact.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	artifact.SizeBytes = int64(len(content))
	if err := artifact.ValidateAttachmentTarget(); err != nil {
		return core.Artifact{}, err
	}
	if artifact.Role == core.ArtifactRoleTypedVerificationEvidence {
		if artifact.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("typed evidence requires a task")
		}
		if _, err := core.ValidateTypedVerificationArtifact(artifact.ContentType, content); err != nil {
			return core.Artifact{}, err
		}
	}
	if artifact.Role == core.ArtifactRoleVerificationEvidence {
		if artifact.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("verification evidence must be attached directly to one task")
		}
		normalized, err := core.ValidateVerificationEvidenceArtifact(artifact.ContentType, content)
		if err != nil {
			return core.Artifact{}, err
		}
		artifact.ContentType = normalized
	}
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	artifact.Workspace = workspace(ctx)
	_, err := tx.Exec(ctx, `INSERT INTO artifacts (id,workspace_id,name,content_type,size_bytes,content,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(workspace_id,id) DO NOTHING`, artifact.ID, workspace(ctx), artifact.Name, artifact.ContentType, artifact.SizeBytes, content, artifact.CreatedAt)
	if err != nil {
		return core.Artifact{}, err
	}
	if artifact.TaskID != "" || artifact.RequirementID != "" || artifact.PlanningSessionID != "" {
		var belongs bool
		switch {
		case artifact.TaskID != "":
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1 AND workspace_id=$2)`, artifact.TaskID, workspace(ctx)).Scan(&belongs)
		case artifact.RequirementID != "":
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM requirements WHERE id=$1 AND workspace_id=$2)`, artifact.RequirementID, workspace(ctx)).Scan(&belongs)
		default:
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM planning_sessions WHERE id=$1 AND workspace_id=$2)`, artifact.PlanningSessionID, workspace(ctx)).Scan(&belongs)
		}
		if err != nil {
			return core.Artifact{}, err
		}
		if !belongs {
			return core.Artifact{}, fmt.Errorf("artifact attachment does not belong to workspace %s", workspace(ctx))
		}
		_, err = tx.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,task_id,requirement_id,planning_session_id,role) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6) ON CONFLICT DO NOTHING`, workspace(ctx), artifact.ID, artifact.TaskID, artifact.RequirementID, artifact.PlanningSessionID, artifact.Role)
		if err != nil {
			return core.Artifact{}, err
		}
	}
	err = tx.QueryRow(ctx, `SELECT name,content_type,size_bytes,created_at FROM artifacts WHERE workspace_id=$1 AND id=$2`, workspace(ctx), artifact.ID).Scan(&artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt)
	return artifact, err
}

func (s *Store) CreateClaimedVerificationEvidence(ctx context.Context, request store.ClaimedVerificationEvidenceRequest, content []byte) (core.Artifact, error) {
	artifact := core.Artifact{
		Workspace: workspace(ctx), Name: request.Name, ContentType: request.ContentType,
		SizeBytes: int64(len(content)), Role: core.ArtifactRoleVerificationEvidence,
		CreatedAt: time.Now().UTC(),
	}
	clientTokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(request.ClientToken)))

	tx, err := s.begin(ctx)
	if err != nil {
		return core.Artifact{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if strings.TrimSpace(request.WorkOrderID) == "" || strings.TrimSpace(request.WorkerID) == "" ||
		strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.ClientToken) == "" {
		return core.Artifact{}, store.ErrVerificationEvidenceClaimConflict
	}
	if err = tx.QueryRow(ctx, `SELECT task_id FROM work_orders
		WHERE workspace_id=$1 AND id=$2 AND state='claimed' AND stage='implement'
		AND worker_id=$3 AND session_id=$4 AND client_token_hash=$5
		AND lease_expires_at>now() AND (execution_deadline IS NULL OR execution_deadline>now())
		FOR UPDATE`, workspace(ctx), request.WorkOrderID, request.WorkerID, request.SessionID, clientTokenHash).Scan(&artifact.TaskID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Artifact{}, store.ErrVerificationEvidenceClaimConflict
		}
		return core.Artifact{}, err
	}
	artifact, err = s.createArtifactTx(ctx, tx, artifact, content)
	if err != nil {
		return core.Artifact{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return core.Artifact{}, err
	}
	return artifact, nil
}

func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := s.boundary.QueryRow(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 AND a.id=$2 ORDER BY (l.role='typed_verification_evidence') DESC,l.role LIMIT 1`, workspace(ctx), id).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID)
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
	err := s.boundary.QueryRow(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,l.role,COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'')
		FROM artifacts a
		JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		WHERE a.workspace_id=$1 AND a.id=$2 AND l.planning_session_id=$3
		ORDER BY l.role LIMIT 1`, workspace(ctx), id, sessionID).Scan(
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
	rows, err := s.boundary.Query(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 ORDER BY a.created_at,a.id,l.role`, workspace(ctx))
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

const listArtifactsForLineageSQL = `WITH wanted(node_type,node_id,ord) AS (
	SELECT node_type,node_id,ord::int FROM unnest($2::text[],$3::text[]) WITH ORDINALITY AS w(node_type,node_id,ord)
), matched AS (
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='task' AND l.workspace_id=$1 AND l.task_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='requirement' AND l.workspace_id=$1 AND l.requirement_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='planning_session' AND l.workspace_id=$1 AND l.planning_session_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifacts a ON w.node_type='evidence' AND a.workspace_id=$1 AND a.id=w.node_id JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		AND l.role='verification_evidence' AND l.task_id IS NOT NULL
		AND ((a.content_type IN ('image/png','image/jpeg','image/webp') AND a.size_bytes BETWEEN 1 AND 10485760)
			OR (a.content_type IN ('video/mp4','video/webm') AND a.size_bytes BETWEEN 1 AND 26214400))
), dedup AS (
	SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,requirement_id,planning_session_id,min(ord) AS ord
	FROM matched GROUP BY id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,requirement_id,planning_session_id
)
SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,
	COALESCE(task_id,''),COALESCE(requirement_id,''),COALESCE(planning_session_id,'')
FROM dedup ORDER BY ord,created_at,id,role`

func (s *Store) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	types, ids := make([]string, 0, len(nodes)), make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Valid() {
			types = append(types, string(node.Type))
			ids = append(ids, node.ID)
		}
	}
	if len(types) == 0 {
		return []core.Artifact{}, nil
	}
	rows, err := s.boundary.Query(ctx, listArtifactsForLineageSQL, workspace(ctx), types, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err = rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}
