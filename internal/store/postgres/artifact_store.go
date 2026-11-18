package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateArtifact")
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

func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetArtifact")
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
	ctx = dbmetrics.WithReadLabel(ctx, "GetArtifactForPlanningSession")
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

// ListArtifacts is two statements for a nonempty workspace, however many
// artifacts or associations exist, as on SingleStore. The first fixes
// (created_at,id) of the newest artifact as a high-water key; the second is
// one complete metadata and association listing bounded by that inclusive
// key. No transaction spans the two statements: artifacts created after the
// fence carry a later key and are excluded even if they commit before the
// listing, and association rows are as of the single listing statement.
// Production writers stamp created_at from the server clock.
func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListArtifacts")
	ws := workspace(ctx)
	var snapAt time.Time
	var snapID string
	err := s.boundary.QueryRow(ctx, `SELECT created_at,id FROM artifacts WHERE workspace_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, ws).Scan(&snapAt, &snapID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	store.RunArtifactListPageHook()
	rows, err := s.boundary.Query(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 AND (a.created_at<$2 OR (a.created_at=$2 AND a.id<=$3)) ORDER BY a.created_at,a.id,l.role`, ws, snapAt, snapID)
	if err != nil {
		return nil, err
	}
	return scanArtifactListRows(rows)
}

func (s *Store) ListArtifactsForOwners(ctx context.Context, owners store.ArtifactOwners) ([]core.Artifact, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListArtifactsForOwners")
	tasks := store.ArtifactOwnerIDs(owners.TaskIDs)
	requirements := store.ArtifactOwnerIDs(owners.RequirementIDs)
	sessions := store.ArtifactOwnerIDs(owners.SessionIDs)
	if len(tasks)+len(requirements)+len(sessions) == 0 {
		return []core.Artifact{}, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifact_links l JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id WHERE l.workspace_id=$1 AND (l.task_id=ANY($2::text[]) OR l.requirement_id=ANY($3::text[]) OR l.planning_session_id=ANY($4::text[])) ORDER BY a.created_at,a.id,l.role`, workspace(ctx), tasks, requirements, sessions)
	if err != nil {
		return nil, err
	}
	result, err := scanArtifactListRows(rows)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []core.Artifact{}, nil
	}
	return result, nil
}

func scanArtifactListRows(rows pgx.Rows) ([]core.Artifact, error) {
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
