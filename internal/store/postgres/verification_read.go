package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// feature-verification-kit-execution VK-9 / DEC-43: this boundary never calls ListVerificationRecords or the snapshot reader.
func (s *Store) ReadVerificationPage(ctx context.Context, a store.VerificationAccess, p store.VerificationPageRequest) (store.VerificationReadPage, error) {
	cursor, err := store.ValidateVerificationPage(ctx, a, &p)
	if err != nil {
		return store.VerificationReadPage{}, err
	}
	if err = store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationReadPage{}, err
	}
	var result store.VerificationReadPage
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationScopeTx(ctx, tx, a, false); err != nil {
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
		result = store.VerificationPageResult(ctx, a, p, items)
		return nil
	})
	return result, err
}

func (s *Store) ReadVerificationDetail(ctx context.Context, a store.VerificationAccess, contextID, evidenceID string) (store.VerificationEvidenceRecord, error) {
	if a.UserID == "" {
		return store.VerificationEvidenceRecord{}, store.ErrVerificationAccess
	}
	if err := store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationEvidenceRecord{}, err
	}
	var result store.VerificationEvidenceRecord
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := s.verificationScopeTx(ctx, tx, a, false); err != nil {
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
