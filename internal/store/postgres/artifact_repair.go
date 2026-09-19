package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) RepairArtifactMetadata(ctx context.Context, r store.ArtifactRepairRequest) (store.ArtifactRepairResult, error) {
	var result store.ArtifactRepairResult
	r, err := store.NormalizeArtifactRepair(ctx, r)
	if err != nil {
		return result, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Request lock precedes artifact row lock (component-persistence ART-STORE-3).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "artifact-repair:"+workspace(ctx)+":"+r.RequestID); err != nil {
			return err
		}
		var prior store.ArtifactRepairRequest
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT artifact_id,expected_old_content_type,new_content_type,result FROM artifact_metadata_repairs WHERE workspace_id=$1 AND request_id=$2`, workspace(ctx), r.RequestID).Scan(&prior.ArtifactID, &prior.ExpectedOldContentType, &prior.NewContentType, &raw)
		if err == nil {
			prior.RequestID = r.RequestID
			if !store.SameArtifactRepair(prior, r) {
				return store.ErrArtifactRepairConflict
			}
			if !r.DryRun {
				return json.Unmarshal(raw, &result)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var a core.Artifact
		var content []byte
		err = tx.QueryRow(ctx, `SELECT id,content_type,size_bytes,content FROM artifacts WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), r.ArtifactID).Scan(&a.ID, &a.ContentType, &a.SizeBytes, &content)
		if err != nil {
			return notFound(err, "artifact %s", r.ArtifactID)
		}
		result, err = store.ValidateArtifactRepair(r, a, content)
		if err != nil || r.DryRun {
			return err
		}
		if result.WouldChange {
			if _, err = tx.Exec(ctx, `UPDATE artifacts SET content_type=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), a.ID, r.NewContentType); err != nil {
				return err
			}
			e := store.ArtifactRepairEvent(ctx, r, a, result)
			if _, err = q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: workspace(ctx), Kind: e.Kind, ActorID: e.ActorID, ActorRole: string(e.ActorRole), PayloadJson: e.Payload, At: timestamp(time.Now().UTC())}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO artifact_metadata_repairs(workspace_id,request_id,artifact_id,expected_old_content_type,new_content_type,result,actor) VALUES($1,$2,$3,$4,$5,$6,$7)`, workspace(ctx), r.RequestID, r.ArtifactID, r.ExpectedOldContentType, r.NewContentType, core.JSONPayload(result), store.ActorFromContext(ctx).ID)
		return err
	})
	return result, err
}
