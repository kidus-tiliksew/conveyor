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

// verificationReadTx runs one inspection read in a read-only REPEATABLE READ
// transaction. It takes no advisory or row lock, so a read never waits for a
// verification command or a held task side-effect lock, and every query sees
// one snapshot (component-verification-evidence "Bounded task inspection reads").
func (s *Store) verificationReadTx(ctx context.Context, fn func(pgx.Tx, *db.Queries) error) error {
	tx, err := s.beginWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return translateBackendConflict(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx, s.queries.WithTx(tx)); err != nil {
		return translateBackendConflict(err)
	}
	return translateBackendConflict(tx.Commit(ctx))
}

// verificationReadScopeTx is the inspection twin of verificationScopeTx. It
// checks membership, task and order ownership, and claim binding inside the
// read transaction with plain SELECTs; mutations keep verificationScopeTx and
// its lock order (component-verification-service).
func (s *Store) verificationReadScopeTx(ctx context.Context, tx pgx.Tx, q *db.Queries, a store.VerificationAccess) (core.WorkOrder, error) {
	ws, ok := store.WorkspaceFromContext(ctx)
	if !ok {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	if err := store.AuthorizeVerificationUserRead(ctx, verificationReadMembership{Store: s, tx: tx}, a); err != nil {
		return core.WorkOrder{}, err
	}
	var found string
	if err := tx.QueryRow(ctx, "SELECT id FROM tasks WHERE workspace_id=$1 AND id=$2", ws, a.TaskID).Scan(&found); err != nil {
		return core.WorkOrder{}, store.ErrVerificationAccess
	}
	var order core.WorkOrder
	if a.UserID == "" || a.WorkOrderID != "" {
		var err error
		order, err = scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", ws, a.WorkOrderID))
		if err != nil {
			return core.WorkOrder{}, store.ErrVerificationAccess
		}
	}
	if a.UserID == "" {
		task, err := q.GetTask(ctx, db.GetTaskParams{ID: found, WorkspaceID: ws})
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = store.VerifyVerificationClaim(ctx, a, taskFromDB(task), order, false, time.Now().UTC()); err != nil {
			return core.WorkOrder{}, err
		}
	}
	verificationReadScoped(ctx)
	return order, nil
}

// verificationReadMembership answers the shared member-read rule from the
// read transaction's snapshot instead of a separate pooled query.
type verificationReadMembership struct {
	*Store
	tx pgx.Tx
}

func (m verificationReadMembership) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	var role core.WorkspaceRole
	err := m.tx.QueryRow(ctx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return core.RoleAllows(role, capability), nil
}

// verificationReadScopedKey carries a test-only callback that runs after the
// read scope is authorized and before any record is read. Production contexts
// carry none.
type verificationReadScopedKey struct{}

func verificationReadScoped(ctx context.Context) {
	if fn, ok := ctx.Value(verificationReadScopedKey{}).(func()); ok {
		fn()
	}
}

// component-verification-evidence / DEC-43: this boundary never calls ListVerificationRecords or the snapshot reader.
func (s *Store) ReadVerificationPage(ctx context.Context, a store.VerificationAccess, p store.VerificationPageRequest) (store.VerificationReadPage, error) {
	cursor, err := store.ValidateVerificationPage(ctx, a, &p)
	if err != nil {
		return store.VerificationReadPage{}, err
	}
	var result store.VerificationReadPage
	err = s.verificationReadTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationReadScopeTx(ctx, tx, q, a); err != nil {
			return err
		}
		if p.ContextID != "" {
			var found string
			if err := tx.QueryRow(ctx, `SELECT id FROM verification_contexts WHERE workspace_id=$1 AND task_id=$2 AND id=$3`, workspace(ctx), a.TaskID, p.ContextID).Scan(&found); err != nil {
				return store.ErrVerificationAccess
			}
		}
		rows, err := q.ListVerificationReadPage(ctx, workspace(ctx), a.TaskID, p.Kind, p.ContextID, cursor.At, cursor.ID, p.Limit+1)
		if err != nil {
			return err
		}
		items := make([]store.VerificationReadItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, store.VerificationReadItem{ID: r.ID, ContextID: r.ContextID, RunID: r.RunID, State: r.State, At: r.At, Metadata: r.Metadata})
		}
		if p.Kind == "publications" {
			for i := range items {
				d, found, e := readDeliveryMetadataTx(ctx, tx, a.TaskID, items[i].ContextID, items[i].ID)
				if e != nil {
					return e
				}
				if found {
					if e = store.JoinVerificationDelivery(&items[i], d); e != nil {
						return e
					}
				}
			}
		}
		result = store.VerificationPageResult(ctx, a, p, items)
		return nil
	})
	return result, err
}

func (s *Store) ReadVerificationDetail(ctx context.Context, a store.VerificationAccess, contextID, evidenceID string) (store.VerificationEvidenceRecord, error) {
	if a.UserID == "" {
		return store.VerificationEvidenceRecord{}, store.ErrVerificationAccess
	}
	var result store.VerificationEvidenceRecord
	err := s.verificationReadTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationReadScopeTx(ctx, tx, q, a); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT e.body FROM verification_evidence e JOIN verification_contexts c ON c.workspace_id=e.workspace_id AND c.task_id=e.task_id AND c.id=e.context_id WHERE e.workspace_id=$1 AND e.task_id=$2 AND e.context_id=$3 AND e.id=$4 AND e.state='evidence'`, workspace(ctx), a.TaskID, contextID, evidenceID).Scan(&raw); err != nil {
			return store.ErrVerificationAccess
		}
		if json.Unmarshal(raw, &result) != nil || store.VerificationEvidenceDigest(result.Envelope) != result.Digest {
			return store.ErrVerificationInvalid
		}
		return nil
	})
	if err != nil {
		return store.VerificationEvidenceRecord{}, err
	}
	return result, nil
}

func readDeliveryMetadataTx(ctx context.Context, tx pgx.Tx, task, contextID, source string) (core.VerificationDelivery, bool, error) {
	var d core.VerificationDelivery
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT body - 'Summary' FROM verification_publication_deliveries WHERE workspace_id=$1 AND task_id=$2 AND context_id=$3 AND source_publication_id=$4 ORDER BY generation DESC LIMIT 1`, workspace(ctx), task, contextID, source).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	err = json.Unmarshal(raw, &d)
	return d, err == nil, err
}
