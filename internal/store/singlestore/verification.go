package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
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

		if c.Kind == store.VerificationWriteEvidence || mutation.Publication != nil {
			sourceID := ""
			if mutation.Publication != nil {
				sourceID = mutation.Publication.ID
			}
			if err = s.deliveryIntentTx(ctx, tx, c.Access.TaskID, append(rows, mutation.Rows...), sourceID); err != nil {
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
		if err != nil {
			return err
		}
		for i := range result.Publications {
			p := &result.Publications[i]
			d, found, e := readDeliveryMetadataTx(ctx, tx, a.TaskID, p.ContextID, p.ID)
			if e != nil {
				return e
			}
			if found {
				p.Delivery = &d
			}
		}
		return nil
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

func deliveryRowsTx(ctx context.Context, tx *sql.Tx, ws, key string) ([]core.VerificationDelivery, error) {
	rows, err := tx.QueryContext(ctx, `SELECT body FROM verification_publication_deliveries WHERE workspace_id=? AND pr_key=? ORDER BY generation`, ws, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ds []core.VerificationDelivery
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d core.VerificationDelivery
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}
func putDelivery(ctx context.Context, tx *sql.Tx, d core.VerificationDelivery) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(body) > 32768 {
		return store.ErrVerificationInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO verification_publication_deliveries(workspace_id,id,task_id,context_id,source_publication_id,pr_key,generation,state,body,next_attempt_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE state=VALUES(state),body=VALUES(body),next_attempt_at=VALUES(next_attempt_at)`, d.WorkspaceID, d.ID, d.TaskID, d.ContextID, d.SourcePublicationID, store.VerificationDeliveryKey(d.Repository, d.PullRequestNumber), d.Generation, d.State, body, d.NextAttemptAt)
	return err
}
func (s *Store) deliveryIntentTx(ctx context.Context, tx *sql.Tx, task string, rows []store.VerificationRow, sourceID string) error {
	ws := documentWorkspace(ctx)
	events, err := documentTaskEvents(ctx, tx, task)
	if err != nil {
		return err
	}
	pr, found := store.VerificationPRFromEvents(events)
	if !found {
		return nil
	}
	if err = lockKey(ctx, tx, "verification-publication:"+ws+":"+store.VerificationDeliveryKey(pr.Repository, pr.Number)); err != nil {
		return err
	}
	prior, err := deliveryRowsTx(ctx, tx, ws, store.VerificationDeliveryKey(pr.Repository, pr.Number))
	if err != nil {
		return err
	}
	added, updates, err := store.PrepareVerificationDelivery(ws, task, pr, rows, prior, sourceID, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, v := range added {
		if err = verificationPutTx(ctx, tx, ws, v); err != nil {
			return err
		}
	}
	for _, d := range updates {
		if err = putDelivery(ctx, tx, d); err != nil {
			return err
		}
		if d.State == "pending" {
			a := store.VerificationDeliveryArgs(d)
			if _, err = logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, ws, a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) RecordVerificationPullRequest(ctx context.Context, e core.Event) error {
	if _, ok := store.WorkspaceFromContext(ctx); !ok || e.Kind != "pull_request.opened" {
		return store.ErrVerificationAccess
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.verificationScopeTx(ctx, tx, store.VerificationAccess{TaskID: e.TaskID, UserID: "internal-publication"}, false); err != nil {
			return err
		}
		if e.JobID != "" {
			var task string
			if err := tx.QueryRowContext(ctx, `SELECT task_id FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), e.JobID).Scan(&task); err != nil || task != e.TaskID {
				return store.ErrVerificationAccess
			}
		}
		if err := insertEvent(ctx, tx, e); err != nil {
			return err
		}
		rows, err := verificationRowsTx(ctx, tx, documentWorkspace(ctx), e.TaskID)
		if err != nil {
			return err
		}
		if err = s.deliveryIntentTx(ctx, tx, e.TaskID, rows, ""); err != nil {
			return err
		}
		return store.VerificationFault(ctx, "queue")
	})
}
func (s *Store) TranslateVerificationPublication(ctx context.Context, p store.VerificationPublication) error {
	if _, ok := store.WorkspaceFromContext(ctx); !ok {
		return store.ErrVerificationAccess
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.verificationScopeTx(ctx, tx, store.VerificationAccess{TaskID: p.TaskID, UserID: "internal-publication"}, false); err != nil {
			return err
		}
		rows, err := verificationRowsTx(ctx, tx, documentWorkspace(ctx), p.TaskID)
		if err != nil {
			return err
		}
		return s.deliveryIntentTx(ctx, tx, p.TaskID, rows, p.ID)
	})
}
func (s *Store) RunVerificationDelivery(ctx context.Context, a queue.VerificationPublicationArgs, fn func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error) error {
	ws, err := workspace(ctx)
	if err != nil || !a.ValidWorkspace(ws) {
		return store.ErrVerificationAccess
	}
	var tx *sql.Tx
	key := store.VerificationDeliveryKey(a.Repository, a.PullRequestNumber)
	begin := func() error {
		var e error
		tx, e = s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		return lockKey(ctx, tx, "verification-publication:"+ws+":"+key)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err = begin(); err != nil {
		return err
	}
	load := func() (core.VerificationDelivery, bool, error) {
		ds, e := deliveryRowsTx(ctx, tx, ws, key)
		if e != nil {
			return core.VerificationDelivery{}, false, e
		}
		d, ok := store.VerificationDeliveryLatest(ds)
		return d, ok, nil
	}
	current, ok, err := load()
	if err != nil {
		return err
	}
	if !ok {
		return tx.Commit()
	}
	view := current
	check := func() error {
		latest, ok, e := load()
		if e != nil {
			return e
		}
		if !ok || latest.ID != current.ID || latest.Generation != current.Generation {
			return store.ErrVerificationConflict
		}
		return nil
	}
	save := func(command string, next core.VerificationDelivery) error {
		if command == "check" {
			return check()
		}
		updated, e := store.AdvanceVerificationDelivery(current, command, next, time.Now().UTC())
		if e != nil {
			return e
		}
		if e = putDelivery(ctx, tx, updated); e != nil {
			return e
		}
		current = updated
		view = updated
		if command == "attempt" {
			if e = tx.Commit(); e != nil {
				return e
			}
			tx = nil
			if e = begin(); e != nil {
				return e
			}
			return check()
		}
		return nil
	}
	if err = fn(&view, save); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ReconcileVerificationDeliveries(ctx context.Context) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "verification:"+ws); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT body FROM verification_publication_deliveries WHERE workspace_id=? AND (state IN ('pending','retrying') OR state='failed' AND next_attempt_at<=?) ORDER BY pr_key,generation LIMIT 100`, ws, time.Now().UTC())
		if err != nil {
			return err
		}
		var ds []core.VerificationDelivery
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			var d core.VerificationDelivery
			if err = json.Unmarshal(raw, &d); err != nil {
				rows.Close()
				return err
			}
			ds = append(ds, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, d := range ds {
			if d.State == "failed" {
				next, e := store.AdvanceVerificationDelivery(d, "retry", d, time.Now().UTC())
				if e != nil {
					return e
				}
				if e = putDelivery(ctx, tx, next); e != nil {
					return e
				}
				d = next
			}
			a := store.VerificationDeliveryArgs(d)
			if _, err = logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, ws, a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
}
