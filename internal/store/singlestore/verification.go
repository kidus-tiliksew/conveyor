package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func verificationRowsTx(ctx context.Context, tx *sql.Tx, ws, task string) ([]store.VerificationRow, error) {
	var out []store.VerificationRow
	for _, table := range store.VerificationTables {
		condition := "task_id=?"
		if table == "verification_operations" {
			condition = "(task_id=? OR true)"
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,task_id,context_id,run_id,logical_key,state,body,expires_at FROM "+table+" WHERE workspace_id=? AND "+condition+" ORDER BY id", ws, task)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			v := store.VerificationRow{Table: table}
			if err = rows.Scan(&v.ID, &v.TaskID, &v.ContextID, &v.RunID, &v.LogicalKey, &v.State, &v.Body, &v.ExpiresAt); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func verificationPutTx(ctx context.Context, tx *sql.Tx, ws string, v store.VerificationRow) error {
	allowed := false
	for _, table := range store.VerificationTables {
		if v.Table == table {
			allowed = true
		}
	}
	if !allowed {
		return store.ErrVerificationInvalid
	}
	suffix := ""
	switch v.Table {
	case "verification_contexts", "verification_attempts", "verification_operations":
		suffix = " ON DUPLICATE KEY UPDATE state=VALUES(state),body=VALUES(body)"
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+v.Table+" (workspace_id,id,task_id,context_id,run_id,logical_key,key_hash,state,body,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?)"+suffix, ws, v.ID, v.TaskID, v.ContextID, v.RunID, v.LogicalKey, fmt.Sprintf("%x", sha256.Sum256([]byte(store.VerificationStorageKey(v)))), v.State, []byte(v.Body), v.ExpiresAt)
	return err
}
func (s *Store) verificationScopeTx(ctx context.Context, tx *sql.Tx, a store.VerificationAccess, write bool, reconcile ...bool) (core.WorkOrder, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if err = s.lockTaskOperation(ctx, tx, ws, a.TaskID); err != nil {
		return core.WorkOrder{}, err
	}
	if err = lockKey(ctx, tx, "verification:"+ws); err != nil {
		return core.WorkOrder{}, err
	}
	var found string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE", ws, a.TaskID).Scan(&found); err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if a.UserID != "" && a.WorkOrderID == "" {
		return core.WorkOrder{}, nil
	}
	order, err := getOrderRow(ctx, tx, a.WorkOrderID)
	if err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if a.UserID != "" {
		return order, nil
	}
	if len(reconcile) == 1 && reconcile[0] {
		err = store.VerifyVerificationClaimLoss(ctx, a, core.Task{ID: found, Workspace: ws}, order, time.Now().UTC())
	} else {
		task, e := getTaskRow(ctx, tx, found)
		if e != nil {
			return core.WorkOrder{}, e
		}
		err = store.VerifyVerificationClaim(ctx, a, task, order, write, time.Now().UTC())
	}
	return order, err
}

type verificationSecretSnapshot []string

func (s verificationSecretSnapshot) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s, nil
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
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		order, err := s.verificationScopeTx(ctx, tx, c.Access, c.Kind != store.VerificationSeal, c.Kind == store.VerificationReconcileClaimLoss)
		if err != nil {
			return err
		}
		if store.VerificationPermissionCommand(c) {
			tr, e := getTaskRow(ctx, tx, c.Access.TaskID)
			if e != nil {
				return e
			}
			if e = store.BindVerificationPermissionOrder(ctx, &c, tr, order, time.Now().UTC()); e != nil {
				return e
			}
		}
		if err = store.VerifyVerificationAuthority(c, authority, order); err != nil {
			return err
		}
		ws := documentWorkspace(ctx)
		rows, err := verificationRowsTx(ctx, tx, ws, c.Access.TaskID)
		if err != nil {
			return err
		}
		mutation, err := store.PrepareVerificationMutation(ctx, verificationSecretSnapshot(secrets), c, rows, time.Now().UTC())
		if err != nil {
			return err
		}
		if c.Kind == store.VerificationSeal && len(mutation.Rows) > 0 {
			c = store.VerificationSealedCommand(c, mutation)
			if err = s.completeVerificationTx(ctx, tx, c, order, rows); err != nil {
				return err
			}
		}
		result = mutation.Receipt
		if len(mutation.Rows) == 0 && len(mutation.DeleteChunks) == 0 {
			return nil
		}
		for _, a := range mutation.Artifacts {
			artifact, err := prepareArtifact(ctx, a.Artifact, a.Content)
			if err != nil {
				return err
			}
			if err = insertArtifactTx(ctx, tx, artifact, a.Content); err != nil {
				return err
			}
		}
		for _, ref := range mutation.ArtifactReferences {
			var content []byte
			if err = tx.QueryRowContext(ctx, "SELECT content FROM artifacts WHERE workspace_id=? AND id=?", ws, ref.ArtifactID).Scan(&content); err != nil {
				return store.ErrVerificationAccess
			}
			if err = store.VerifyRetainedVerificationArtifact(ref, content); err != nil {
				return err
			}
		}
		if err = store.VerificationFault(ctx, "artifact"); err != nil {
			return err
		}
		for _, row := range mutation.Rows {
			if err = verificationPutTx(ctx, tx, ws, row); err != nil {
				return err
			}
		}
		for _, id := range mutation.DeleteChunks {
			if _, err = tx.ExecContext(ctx, "DELETE FROM verification_upload_chunks WHERE workspace_id=? AND id=?", ws, id); err != nil {
				return err
			}
		}
		if err = store.VerificationFault(ctx, "evidence"); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, mutation.Event); err != nil {
			return err
		}
		if err = store.VerificationFault(ctx, "event"); err != nil {
			return err
		}
		if p := mutation.Publication; p != nil {
			if _, err = logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, ws, "verification_publication", p.ID, p, 5, time.Now().UTC()); err != nil {
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
func (s *Store) ReadVerification(ctx context.Context, a store.VerificationAccess, id string) (store.VerificationSnapshot, error) {
	if err := store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationSnapshot{}, err
	}
	var result store.VerificationSnapshot
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		order, err := s.verificationScopeTx(ctx, tx, a, false)
		if err != nil {
			return err
		}
		rows, err := verificationRowsTx(ctx, tx, documentWorkspace(ctx), a.TaskID)
		if err != nil {
			return err
		}
		result, err = store.VerificationReadSnapshot(rows, a, id, order.Stage)
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
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		order, err := s.verificationScopeTx(ctx, tx, a, false)
		if err != nil {
			return err
		}
		ws := documentWorkspace(ctx)
		rows, err := verificationRowsTx(ctx, tx, ws, a.TaskID)
		if err != nil {
			return err
		}
		if err = store.VerificationArtifactAccess(rows, a, evidenceID, artifactID, order.Stage); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT name,content_type,size_bytes,content,created_at FROM artifacts WHERE workspace_id=? AND id=?", ws, artifactID).Scan(&artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt)
		if err != nil {
			return store.ErrVerificationAccess
		}
		if fmt.Sprintf("%x", sha256.Sum256(content)) != artifactID {
			return fmt.Errorf("verification artifact integrity failure")
		}
		artifact.ContentType = store.VerificationArtifactMedia(rows, evidenceID, artifactID)
		artifact.ID = artifactID
		artifact.Workspace = ws
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
		var rows []store.VerificationRow
		err = s.withTx(ctx, func(tx *sql.Tx) error {
			var e error
			rows, e = verificationRowsTx(ctx, tx, documentWorkspace(ctx), task.ID)
			return e
		})
		if err != nil {
			return count, err
		}
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
