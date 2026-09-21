package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func verificationRows(records []db.VerificationRecord) []store.VerificationRow {
	rows := make([]store.VerificationRow, 0, len(records))
	for _, v := range records {
		rows = append(rows, store.VerificationRow{Table: v.Table, ID: v.ID, TaskID: v.TaskID, ContextID: v.ContextID, RunID: v.RunID, LogicalKey: v.LogicalKey, State: v.State, Body: v.Body, ExpiresAt: v.ExpiresAt})
	}
	return rows
}
func (s *Store) verificationScopeTx(ctx context.Context, tx pgx.Tx, a store.VerificationAccess, write bool, reconcile ...bool) (core.WorkOrder, error) {
	ws, ok := store.WorkspaceFromContext(ctx)
	if !ok {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	// The workspace lock also serializes unresolved operation identities across
	// tasks; task locks remain the lifecycle boundary shared with claim changes.
	if err := lockWorkOrderTaskTx(ctx, tx, ws, a.TaskID); err != nil {
		return core.WorkOrder{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:verification:"+ws); err != nil {
		return core.WorkOrder{}, err
	}
	var found string
	if err := tx.QueryRow(ctx, "SELECT id FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE", ws, a.TaskID).Scan(&found); err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if a.UserID != "" {
		return core.WorkOrder{}, nil
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", ws, a.WorkOrderID))
	if err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if len(reconcile) == 1 && reconcile[0] {
		err = store.VerifyVerificationClaimLoss(ctx, a, core.Task{ID: found, Workspace: ws}, order, time.Now().UTC())
	} else {
		task, e := s.queries.WithTx(tx).GetTask(ctx, db.GetTaskParams{ID: found, WorkspaceID: ws})
		if e != nil {
			return core.WorkOrder{}, e
		}
		err = store.VerifyVerificationClaim(ctx, a, taskFromDB(task), order, write, time.Now().UTC())
	}
	return order, err
}
func (s *Store) ApplyVerification(ctx context.Context, c store.VerificationCommand) (store.VerificationReceipt, error) {
	return taskops.ExecuteVerification(ctx, s, c.Access.TaskID, c.Kind, func(lease taskops.TaskLease) (store.VerificationReceipt, error) {
		return s.applyVerification(ctx, lease, c)
	})
}

func (s *Store) applyVerification(ctx context.Context, lease taskops.TaskLease, c store.VerificationCommand) (store.VerificationReceipt, error) {
	if !lease.ValidForCommand(c.Access.TaskID, "verification."+c.Kind) {
		return store.VerificationReceipt{}, store.ErrVerificationAccess
	}
	// Fetch the redaction source before holding database locks; failure leaves no
	// staged writes. Prepare reads this snapshot while the claim is locked.
	if err := store.BindVerificationEvidenceAuthority(ctx, s, &c); err != nil {
		return store.VerificationReceipt{}, err
	}
	authority, err := store.LoadVerificationAuthority(ctx, s, c)
	if err != nil {
		return store.VerificationReceipt{}, err
	}
	secrets, err := s.ListGitHubAppKeysForRedaction(ctx)
	if err != nil {
		return store.VerificationReceipt{}, err
	}
	var result store.VerificationReceipt
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		order, err := s.verificationScopeTx(ctx, tx, c.Access, c.Kind != store.VerificationSeal, c.Kind == store.VerificationReconcileClaimLoss)
		if err != nil {
			return err
		}
		if err = store.VerifyVerificationAuthority(c, authority, order); err != nil {
			return err
		}
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), c.Access.TaskID)
		if err != nil {
			return err
		}
		mutation, err := store.PrepareVerificationMutation(ctx, verificationSecretSnapshot(secrets), c, verificationRows(records), time.Now().UTC())
		if err != nil {
			return err
		}
		if c.Kind == store.VerificationSeal && len(mutation.Rows) > 0 {
			c = store.VerificationSealedCommand(c, mutation)
			if err = s.completeVerificationTx(ctx, tx, q, c, order, verificationRows(records)); err != nil {
				return err
			}
		}
		result = mutation.Receipt
		if len(mutation.Rows) == 0 && len(mutation.DeleteChunks) == 0 {
			return nil
		}
		for _, a := range mutation.Artifacts {
			if _, err = s.createArtifactTx(ctx, tx, a.Artifact, a.Content); err != nil {
				return err
			}
		}
		for _, ref := range mutation.ArtifactReferences {
			var content []byte
			if err = tx.QueryRow(ctx, "SELECT content FROM artifacts WHERE workspace_id=$1 AND id=$2", workspace(ctx), ref.ArtifactID).Scan(&content); err != nil {
				return store.ErrVerificationAccess
			}
			if err = store.VerifyRetainedVerificationArtifact(ref, content); err != nil {
				return err
			}
		}
		if err = store.VerificationFault(ctx, "artifact"); err != nil {
			return err
		}
		for _, v := range mutation.Rows {
			if err = q.PutVerificationRecord(ctx, db.VerificationRecord{Table: v.Table, WorkspaceID: workspace(ctx), ID: v.ID, TaskID: v.TaskID, ContextID: v.ContextID, RunID: v.RunID, LogicalKey: v.LogicalKey, KeyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(store.VerificationStorageKey(v)))), State: v.State, Body: v.Body, ExpiresAt: v.ExpiresAt}); err != nil {
				return err
			}
		}
		for _, id := range mutation.DeleteChunks {
			if err = q.DeleteVerificationChunk(ctx, workspace(ctx), id); err != nil {
				return err
			}
		}
		if err = store.VerificationFault(ctx, "evidence"); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, mutation.Event); err != nil {
			return err
		}
		if err = store.VerificationFault(ctx, "event"); err != nil {
			return err
		}
		if p := mutation.Publication; p != nil {
			if _, err = logqueue.Enqueue(pglog.WithTx(ctx, tx), s.log, workspace(ctx), "verification_publication", p.ID, p, 5, time.Now().UTC()); err != nil {
				return err
			}
		}
		return store.VerificationFault(ctx, "queue")
	})
	if err != nil {
		return store.VerificationReceipt{}, err
	}
	return result, nil
}

type verificationSecretSnapshot []string

func (s verificationSecretSnapshot) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s, nil
}
func (s *Store) ReadVerification(ctx context.Context, a store.VerificationAccess, id string) (store.VerificationSnapshot, error) {
	if err := store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationSnapshot{}, err
	}
	var result store.VerificationSnapshot
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		order, err := s.verificationScopeTx(ctx, tx, a, false)
		if err != nil {
			return err
		}
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), a.TaskID)
		if err != nil {
			return err
		}
		result, err = store.VerificationReadSnapshot(verificationRows(records), a, id, order.Stage)
		return err
	})
	return result, err
}
func (s *Store) ReadVerificationArtifact(ctx context.Context, a store.VerificationAccess, evidenceID, artifactID string) (core.Artifact, []byte, error) {
	if err := store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return core.Artifact{}, nil, err
	}
	var artifact core.Artifact
	var content []byte
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		order, err := s.verificationScopeTx(ctx, tx, a, false)
		if err != nil {
			return err
		}
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), a.TaskID)
		if err != nil {
			return err
		}
		if err = store.VerificationArtifactAccess(verificationRows(records), a, evidenceID, artifactID, order.Stage); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, "SELECT name,content_type,size_bytes,content,created_at FROM artifacts WHERE workspace_id=$1 AND id=$2", workspace(ctx), artifactID).Scan(&artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt)
		if err != nil {
			return store.ErrVerificationAccess
		}
		if fmt.Sprintf("%x", sha256.Sum256(content)) != artifactID {
			return fmt.Errorf("verification artifact integrity failure")
		}
		artifact.ContentType = store.VerificationArtifactMedia(verificationRows(records), evidenceID, artifactID)
		artifact.ID = artifactID
		artifact.Workspace = workspace(ctx)
		artifact.TaskID = a.TaskID
		artifact.Role = core.ArtifactRoleTypedVerificationEvidence
		return nil
	})
	if err != nil {
		return core.Artifact{}, nil, err
	}
	return artifact, content, nil
}

func (s *Store) ReconcileVerificationClaims(ctx context.Context) (int, error) {
	if _, err := store.VerificationReconciliationCommands(ctx, nil); err != nil {
		return 0, err
	}
	tasks, err := s.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, task := range tasks {
		records, err := s.queries.ListVerificationRecords(ctx, workspace(ctx), task.ID)
		if err != nil {
			return count, err
		}
		rows := verificationRows(records)
		commands, err := store.VerificationReconciliationCommands(ctx, rows)
		if err != nil {
			return count, err
		}
		for _, c := range commands {
			if _, err = s.ApplyVerification(ctx, c); err != nil {
				if errors.Is(err, store.ErrVerificationState) {
					continue
				}
				return count, err
			}
			count++
		}
	}
	return count, nil
}
