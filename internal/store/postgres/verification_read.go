package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
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

func (m verificationReadMembership) AuthorizeWorkspaceUsers(ctx context.Context, userIDs []string, workspaceID string, capability core.Capability) (map[string]bool, error) {
	out := make(map[string]bool, len(userIDs))
	for _, userID := range userIDs {
		out[userID] = false
	}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := m.tx.Query(ctx, `SELECT user_id,role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=ANY($2::text[])`, workspaceID, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var userID string
		var role core.WorkspaceRole
		if err := rows.Scan(&userID, &role); err != nil {
			return nil, err
		}
		out[userID] = core.RoleAllows(role, capability)
	}
	return out, rows.Err()
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
	ctx = dbmetrics.WithReadLabel(ctx, "ReadVerificationPage")
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
		if p.Kind == "publications" && len(items) > 0 {
			sources := make([]string, 0, len(items))
			for _, item := range items {
				sources = append(sources, item.ID)
			}
			deliveries, e := deliveryMetadataBatchTx(ctx, tx, a.TaskID, sources)
			if e != nil {
				return e
			}
			for i := range items {
				if d, found := deliveries[items[i].ID]; found && d.ContextID == items[i].ContextID {
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
	ctx = dbmetrics.WithReadLabel(ctx, "ReadVerificationDetail")
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

// deliveryMetadataColumns is the bounded projection a read joins onto a
// publication: identity and outcome fields only, never the retained body.
const deliveryMetadataColumns = `jsonb_build_object('WorkspaceID',left(body->>'WorkspaceID',2048),'Repository',left(body->>'Repository',2048),'TaskID',left(body->>'TaskID',2048),'ContextID',left(body->>'ContextID',2048),'SourcePublicationID',left(body->>'SourcePublicationID',2048),'ID',left(body->>'ID',2048),'IdempotencyKey',left(body->>'IdempotencyKey',2048),'TargetHead',left(body->>'TargetHead',2048),'ObservedHead',left(body->>'ObservedHead',2048),'TargetDigest',left(body->>'TargetDigest',2048),'ObservedDigest',left(body->>'ObservedDigest',2048),'Summary',left(body->>'Summary',2048),'State',left(body->>'State',2048),'ErrorClass',left(body->>'ErrorClass',2048),'Generation',(body->>'Generation')::bigint,'SourceGeneration',(body->>'SourceGeneration')::bigint,'PullRequestNumber',(body->>'PullRequestNumber')::bigint,'Attempts',(body->>'Attempts')::bigint,'CycleAttempts',(body->>'CycleAttempts')::bigint,'LastAttemptAt',body->'LastAttemptAt','NextAttemptAt',body->'NextAttemptAt')`

// deliveryMetadataBatchTx reads the delivery metadata of many publications in
// one statement, so a snapshot's statement count does not grow with one query
// per publication (component-persistence, Database read accounting).
func deliveryMetadataBatchTx(ctx context.Context, tx pgx.Tx, task string, sources []string) (map[string]core.VerificationDelivery, error) {
	out := map[string]core.VerificationDelivery{}
	for start := 0; start < len(sources); start += verificationMetadataChunk {
		chunk := sources[start:min(start+verificationMetadataChunk, len(sources))]
		args := make([]any, 0, 2+len(chunk))
		args = append(args, workspace(ctx), task)
		args = append(args, verificationMetadataArgs(chunk)...)
		// One statement over the chunk; the newest generation per publication is
		// chosen here, so no pre-query narrows anything.
		rows, err := tx.Query(ctx, `SELECT source_publication_id, `+deliveryMetadataColumns+` FROM verification_publication_deliveries WHERE workspace_id=$1 AND task_id=$2 AND source_publication_id IN (`+verificationPlaceholders(3, len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source string
			var raw []byte
			if err = rows.Scan(&source, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var d core.VerificationDelivery
			if err = json.Unmarshal(raw, &d); err != nil {
				rows.Close()
				return nil, err
			}
			if prior, found := out[source]; found && prior.Generation >= d.Generation {
				continue
			}
			out[source] = d
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	store.NoteVerificationRead(store.VerificationReadMetadata, workspace(ctx), task, len(out))
	return out, nil
}

// verificationMetadataChunk bounds one statement's publication list.
const verificationMetadataChunk = 500

// verificationPlaceholders numbers count arguments from first.
func verificationPlaceholders(first, count int) string {
	marks := make([]string, count)
	for i := range marks {
		marks[i] = fmt.Sprintf("$%d", first+i)
	}
	return strings.Join(marks, ",")
}

func verificationMetadataArgs(sources []string) []any {
	args := make([]any, 0, len(sources))
	for _, source := range sources {
		args = append(args, source)
	}
	return args
}
