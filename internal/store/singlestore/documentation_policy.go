package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) PinTaskDocumentationPolicy(ctx context.Context, taskID string, policy core.DocumentationPolicy) (bool, error) {
	if err := store.ValidateDocumentationPolicyPin(policy); err != nil {
		return false, err
	}
	result, err := documentExec(ctx, s.db, `INSERT IGNORE INTO task_documentation_policies (workspace_id, task_id, payload_json) VALUES (?,?,?)`,
		documentWorkspace(ctx), taskID, core.JSONPayload(store.NormalizeDocumentationPolicy(policy)))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) RecordDocumentationGateEvidence(ctx context.Context, evidence core.DocumentationGateEvidence) error {
	normalized, err := store.NormalizeDocumentationGateEvidence(evidence)
	if err != nil {
		return err
	}
	_, err = documentExec(ctx, s.db, `INSERT INTO documentation_gate_evidence (workspace_id, task_id, head_sha, payload_json) VALUES (?,?,?,?)
		ON DUPLICATE KEY UPDATE payload_json = VALUES(payload_json)`,
		documentWorkspace(ctx), normalized.TaskID, normalized.HeadSHA, core.JSONPayload(normalized))
	return err
}

func (s *Store) GetDocumentationGateEvidence(ctx context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, bool, error) {
	var raw []byte
	var evidence core.DocumentationGateEvidence
	err := documentRow(ctx, s.db, `SELECT payload_json FROM documentation_gate_evidence WHERE workspace_id=? AND task_id=? AND head_sha=?`,
		documentWorkspace(ctx), taskID, headSHA).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence, false, nil
	}
	if err != nil {
		return evidence, false, err
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		return evidence, false, err
	}
	return evidence, true, nil
}

func (s *Store) hydrateDocumentationPolicy(ctx context.Context, task *core.Task) error {
	var raw []byte
	err := documentRow(ctx, s.db, `SELECT payload_json FROM task_documentation_policies WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), task.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var policy core.DocumentationPolicy
	if err = json.Unmarshal(raw, &policy); err != nil {
		return err
	}
	task.DocumentationPolicy = &policy
	return nil
}
