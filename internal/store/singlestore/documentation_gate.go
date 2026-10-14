package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) RecordDocumentationGateEvidence(ctx context.Context, e core.DocumentationGateEvidence) error {
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	normalized, err := store.NormalizeDocumentationGateEvidence(e)
	if err != nil {
		return err
	}
	if _, err = s.GetTask(ctx, e.TaskID); err != nil {
		return err
	}
	return s.taskTx(ctx, e.TaskID, func(tx *sql.Tx) error {
		_, err := documentExec(ctx, tx, `INSERT INTO documentation_gate_evidence(workspace_id,task_id,head_sha,payload_json) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE payload_json=VALUES(payload_json)`, documentWorkspace(ctx), e.TaskID, e.HeadSHA, core.JSONPayload(normalized))
		return err
	})
}
func (s *Store) GetDocumentationGateEvidence(ctx context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, error) {
	var e core.DocumentationGateEvidence
	var raw []byte
	err := documentRow(ctx, s.db, `SELECT payload_json FROM documentation_gate_evidence WHERE workspace_id=? AND task_id=? AND head_sha=?`, documentWorkspace(ctx), taskID, headSHA).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return e, store.ErrNotFound
	}
	if err != nil {
		return e, err
	}
	err = json.Unmarshal(raw, &e)
	return e, err
}
