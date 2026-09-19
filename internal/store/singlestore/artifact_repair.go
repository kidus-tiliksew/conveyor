package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

var errArtifactRepairPreview = errors.New("artifact repair preview rollback")

func (s *Store) RepairArtifactMetadata(ctx context.Context, r store.ArtifactRepairRequest) (store.ArtifactRepairResult, error) {
	var result store.ArtifactRepairResult
	r, err := store.NormalizeArtifactRepair(ctx, r)
	if err != nil {
		return result, err
	}
	ws, _ := workspace(ctx)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "artifact-repair:"+ws+":"+r.RequestID); err != nil {
			return err
		}
		var prior store.ArtifactRepairRequest
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT artifact_id,expected_old_content_type,new_content_type,result FROM artifact_metadata_repairs WHERE workspace_id=? AND request_id=?`, ws, r.RequestID).Scan(&prior.ArtifactID, &prior.ExpectedOldContentType, &prior.NewContentType, &raw)
		if err == nil {
			prior.RequestID = r.RequestID
			if !store.SameArtifactRepair(prior, r) {
				return store.ErrArtifactRepairConflict
			}
			if !r.DryRun {
				return json.Unmarshal(raw, &result)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = lockKey(ctx, tx, "artifact:"+ws+":"+r.ArtifactID); err != nil {
			return err
		}
		var a core.Artifact
		var content []byte
		err = tx.QueryRowContext(ctx, `SELECT id,content_type,size_bytes,content FROM artifacts WHERE workspace_id=? AND id=? FOR UPDATE`, ws, r.ArtifactID).Scan(&a.ID, &a.ContentType, &a.SizeBytes, &content)
		if err != nil {
			return notFound(err, "artifact %s", r.ArtifactID)
		}
		result, err = store.ValidateArtifactRepair(r, a, content)
		if err != nil {
			return err
		}
		if r.DryRun {
			return errArtifactRepairPreview
		}
		if result.WouldChange {
			if _, err = tx.ExecContext(ctx, `UPDATE artifacts SET content_type=? WHERE workspace_id=? AND id=?`, r.NewContentType, ws, a.ID); err != nil {
				return err
			}
			if strings.ContainsRune(store.ActorFromContext(ctx).ID, 0) {
				return fmt.Errorf("audit actor contains NUL")
			}
			if err = insertWorkspaceEvent(ctx, tx, store.ArtifactRepairEvent(ctx, r, a, result)); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO artifact_metadata_repairs(workspace_id,request_id,artifact_id,expected_old_content_type,new_content_type,result,actor) VALUES(?,?,?,?,?,?,?)`, ws, r.RequestID, r.ArtifactID, r.ExpectedOldContentType, r.NewContentType, core.JSONPayload(result), store.ActorFromContext(ctx).ID)
		return err
	})
	if errors.Is(err, errArtifactRepairPreview) {
		err = nil
	}
	return result, err
}
