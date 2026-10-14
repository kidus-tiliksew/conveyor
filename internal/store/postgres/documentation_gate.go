package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) RecordDocumentationGateEvidence(ctx context.Context, e core.DocumentationGateEvidence) error {
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	normalized, err := store.NormalizeDocumentationGateEvidence(ctx, e)
	if err != nil {
		return err
	}
	if _, err = s.GetTask(ctx, e.TaskID); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		_, err := tx.Exec(ctx, `INSERT INTO documentation_gate_evidence(workspace_id,task_id,head_sha,payload_json) VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,task_id,head_sha) DO UPDATE SET payload_json=EXCLUDED.payload_json`, workspace(ctx), e.TaskID, e.HeadSHA, core.JSONPayload(normalized))
		return err
	})
}
func (s *Store) GetDocumentationGateEvidence(ctx context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, error) {
	var e core.DocumentationGateEvidence
	var raw []byte
	err := s.boundary.QueryRow(ctx, `SELECT payload_json FROM documentation_gate_evidence WHERE workspace_id=$1 AND task_id=$2 AND head_sha=$3`, workspace(ctx), taskID, headSHA).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, store.ErrNotFound
	}
	if err != nil {
		return e, err
	}
	err = json.Unmarshal(raw, &e)
	return e, err
}
