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
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var errArtifactRepairPreview = errors.New("artifact repair preview rollback")

// artifactRepairLedgerHolder is the holder a completed repair records on its
// request's claim row. The claim key already names the ledger row's workspace
// and request ID, so the holder only states that the row was committed.
const artifactRepairLedgerHolder = "artifact_metadata_repairs"

func artifactRepairClaimKey(workspace, requestID string) string {
	return "artifact-repair:" + workspace + ":" + requestID
}

// awaitArtifactRepairLedgerRow reads the request's ledger row. A commit across
// partitions can release the claim row before the ledger row is visible, so a
// holder means the row is committed even when a read misses it, and the read
// repeats until the row appears (component-persistence, SingleStore visibility;
// component-artifacts, Audit and replay). A wait that runs out returns
// store.ErrRetryable, which rolls the transaction back with nothing written.
//
// No holder means no repair committed under the claim, or one committed before
// repairs recorded a holder, whose row is long visible, so one read decides.
func awaitArtifactRepairLedgerRow(ctx context.Context, tx *sql.Tx, ws, requestID, holder string) (prior store.ArtifactRepairRequest, raw []byte, recorded bool, err error) {
	read := func() (bool, error) {
		err := tx.QueryRowContext(ctx, `SELECT artifact_id,expected_old_content_type,new_content_type,result FROM artifact_metadata_repairs WHERE workspace_id=? AND request_id=?`, ws, requestID).Scan(&prior.ArtifactID, &prior.ExpectedOldContentType, &prior.NewContentType, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	if holder == "" {
		recorded, err = read()
		return prior, raw, recorded, err
	}
	recorded, err = awaitClaimedVisibility(ctx, read)
	if err != nil {
		return prior, raw, false, err
	}
	if !recorded {
		return prior, raw, false, fmt.Errorf("%w: recorded artifact repair is not visible", store.ErrRetryable)
	}
	return prior, raw, true, nil
}

func (s *Store) RepairArtifactMetadata(ctx context.Context, r store.ArtifactRepairRequest) (store.ArtifactRepairResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RepairArtifactMetadata")
	var result store.ArtifactRepairResult
	r, err := store.NormalizeArtifactRepair(ctx, r)
	if err != nil {
		return result, err
	}
	ws, _ := workspace(ctx)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		key := artifactRepairClaimKey(ws, r.RequestID)
		holder, err := claimKey(ctx, tx, key)
		if err != nil {
			return err
		}
		prior, raw, recorded, err := awaitArtifactRepairLedgerRow(ctx, tx, ws, r.RequestID, holder)
		if err != nil {
			return err
		}
		if recorded {
			prior.RequestID = r.RequestID
			if !store.SameArtifactRepair(prior, r) {
				return store.ErrArtifactRepairConflict
			}
			if !r.DryRun {
				return json.Unmarshal(raw, &result)
			}
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
		if err != nil {
			return err
		}
		return setClaim(ctx, tx, key, artifactRepairLedgerHolder)
	})
	if errors.Is(err, errArtifactRepairPreview) {
		err = nil
	}
	return result, err
}
