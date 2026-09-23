package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
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
	if a.UserID != "" && a.WorkOrderID == "" {
		return core.WorkOrder{}, nil
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", ws, a.WorkOrderID))
	if err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if a.UserID != "" {
		return order, nil
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
		if store.VerificationPermissionCommand(c) {
			tr, e := q.GetTask(ctx, db.GetTaskParams{ID: c.Access.TaskID, WorkspaceID: workspace(ctx)})
			if e != nil {
				return e
			}
			if e = store.BindVerificationPermissionOrder(ctx, &c, taskFromDB(tr), order, time.Now().UTC()); e != nil {
				return e
			}
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

		if c.Kind == store.VerificationWriteEvidence || mutation.Publication != nil {
			sourceID := ""
			if mutation.Publication != nil {
				sourceID = mutation.Publication.ID
			}
			if err = s.deliveryIntentTx(ctx, tx, q, c.Access.TaskID, append(verificationRows(records), mutation.Rows...), sourceID); err != nil {
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

func deliveryRecords(records []db.VerificationPublicationDeliveryRecord) ([]core.VerificationDelivery, error) {
	out := make([]core.VerificationDelivery, 0, len(records))
	for _, r := range records {
		var d core.VerificationDelivery
		if err := json.Unmarshal(r.Body, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}
func putDelivery(ctx context.Context, q *db.Queries, d core.VerificationDelivery) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(body) > 32768 {
		return store.ErrVerificationInvalid
	}
	return q.PutVerificationPublicationDelivery(ctx, db.VerificationPublicationDeliveryRecord{WorkspaceID: d.WorkspaceID, ID: d.ID, TaskID: d.TaskID, ContextID: d.ContextID, SourcePublicationID: d.SourcePublicationID, PRKey: store.VerificationDeliveryKey(d.Repository, d.PullRequestNumber), Generation: int64(d.Generation), State: d.State, Body: body, NextAttemptAt: d.NextAttemptAt})
}
func (s *Store) deliveryIntentTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task string, rows []store.VerificationRow, sourceID string) error {
	events, err := q.ListEvents(ctx, db.ListEventsParams{WorkspaceID: workspace(ctx), TaskID: nullableText(task)})
	if err != nil {
		return err
	}
	es := make([]core.Event, len(events))
	for i, e := range events {
		es[i] = eventFromDB(e)
	}
	pr, found := store.VerificationPRFromEvents(es)
	if !found {
		return nil
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:verification-publication:"+workspace(ctx)+":"+store.VerificationDeliveryKey(pr.Repository, pr.Number)); err != nil {
		return err
	}
	records, err := q.ListVerificationPublicationDeliveries(ctx, workspace(ctx), store.VerificationDeliveryKey(pr.Repository, pr.Number))
	if err != nil {
		return err
	}
	prior, err := deliveryRecords(records)
	if err != nil {
		return err
	}
	added, updates, err := store.PrepareVerificationDelivery(workspace(ctx), task, pr, rows, prior, sourceID, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, v := range added {
		if err = q.PutVerificationRecord(ctx, db.VerificationRecord{Table: v.Table, WorkspaceID: workspace(ctx), ID: v.ID, TaskID: v.TaskID, ContextID: v.ContextID, RunID: v.RunID, LogicalKey: v.LogicalKey, KeyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(store.VerificationStorageKey(v)))), State: v.State, Body: v.Body}); err != nil {
			return err
		}
	}
	for _, d := range updates {
		if err = putDelivery(ctx, q, d); err != nil {
			return err
		}
		if d.State == "pending" {
			a := store.VerificationDeliveryArgs(d)
			if _, err = logqueue.Enqueue(pglog.WithTx(ctx, tx), s.log, workspace(ctx), a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC()); err != nil {
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
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationScopeTx(ctx, tx, store.VerificationAccess{TaskID: e.TaskID, UserID: "internal-publication"}, false); err != nil {
			return err
		}
		if e.JobID != "" {
			j, err := q.GetJob(ctx, db.GetJobParams{WorkspaceID: workspace(ctx), ID: e.JobID})
			if err != nil || j.TaskID != e.TaskID {
				return store.ErrVerificationAccess
			}
		}
		if err := insertEvent(ctx, q, e); err != nil {
			return err
		}
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), e.TaskID)
		if err != nil {
			return err
		}
		if err = s.deliveryIntentTx(ctx, tx, q, e.TaskID, verificationRows(records), ""); err != nil {
			return err
		}
		return store.VerificationFault(ctx, "queue")
	})
}
func (s *Store) TranslateVerificationPublication(ctx context.Context, p store.VerificationPublication) error {
	if _, ok := store.WorkspaceFromContext(ctx); !ok {
		return store.ErrVerificationAccess
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationScopeTx(ctx, tx, store.VerificationAccess{TaskID: p.TaskID, UserID: "internal-publication"}, false); err != nil {
			return err
		}
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), p.TaskID)
		if err != nil {
			return err
		}
		return s.deliveryIntentTx(ctx, tx, q, p.TaskID, verificationRows(records), p.ID)
	})
}
func (s *Store) RunVerificationDelivery(ctx context.Context, a queue.VerificationPublicationArgs, fn func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error) error {
	if !a.ValidWorkspace(workspace(ctx)) {
		return store.ErrVerificationAccess
	}
	var tx pgx.Tx
	var q *db.Queries
	key := store.VerificationDeliveryKey(a.Repository, a.PullRequestNumber)
	begin := func() error {
		var err error
		tx, err = s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		q = s.queries.WithTx(tx)
		_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:verification-publication:"+workspace(ctx)+":"+key)
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	if err := begin(); err != nil {
		return err
	}
	load := func() (core.VerificationDelivery, bool, error) {
		rs, e := q.ListVerificationPublicationDeliveries(ctx, workspace(ctx), key)
		if e != nil {
			return core.VerificationDelivery{}, false, e
		}
		ds, e := deliveryRecords(rs)
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
		return tx.Commit(ctx)
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
		if e = putDelivery(ctx, q, updated); e != nil {
			return e
		}
		current = updated
		view = updated
		if command == "attempt" {
			// Commit retrying before network access. Reacquire the PR lock and verify
			// the target again, so a generation committed in this gap wins safely.
			if e = tx.Commit(ctx); e != nil {
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
	return tx.Commit(ctx)
}
func (s *Store) ReconcileVerificationDeliveries(ctx context.Context) error {
	if _, ok := store.WorkspaceFromContext(ctx); !ok {
		return store.ErrVerificationAccess
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:verification:"+workspace(ctx)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT body FROM verification_publication_deliveries WHERE workspace_id=$1 AND (state IN ('pending','retrying') OR state='failed' AND next_attempt_at<=$2) ORDER BY pr_key,generation LIMIT 100`, workspace(ctx), time.Now().UTC())
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
				if e = putDelivery(ctx, q, next); e != nil {
					return e
				}
				d = next
			}
			a := store.VerificationDeliveryArgs(d)
			if _, err = logqueue.Enqueue(pglog.WithTx(ctx, tx), s.log, workspace(ctx), a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
}
