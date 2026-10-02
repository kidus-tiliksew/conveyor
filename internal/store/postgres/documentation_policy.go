package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) PinTaskDocumentationPolicy(ctx context.Context, taskID string, policy core.DocumentationPolicy) (bool, error) {
	if err := store.ValidateDocumentationPolicyPin(policy); err != nil {
		return false, err
	}
	payload, err := json.Marshal(store.NormalizeDocumentationPolicy(policy))
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO task_documentation_policies (workspace_id, task_id, payload_json)
		VALUES ($1,$2,$3) ON CONFLICT (workspace_id, task_id) DO NOTHING`, workspace(ctx), taskID, payload)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) RecordDocumentationGateEvidence(ctx context.Context, evidence core.DocumentationGateEvidence) error {
	normalized, err := store.NormalizeDocumentationGateEvidence(evidence)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO documentation_gate_evidence (workspace_id, task_id, head_sha, payload_json)
		VALUES ($1,$2,$3,$4) ON CONFLICT (workspace_id, task_id, head_sha) DO UPDATE SET payload_json = EXCLUDED.payload_json`,
		workspace(ctx), normalized.TaskID, normalized.HeadSHA, payload)
	return err
}

func (s *Store) GetDocumentationGateEvidence(ctx context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, bool, error) {
	var raw []byte
	var evidence core.DocumentationGateEvidence
	err := s.pool.QueryRow(ctx, `SELECT payload_json FROM documentation_gate_evidence WHERE workspace_id=$1 AND task_id=$2 AND head_sha=$3`,
		workspace(ctx), taskID, headSHA).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
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
	raw, err := s.documentationPolicyPayload(ctx, task.ID)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	policy, err := decodeDocumentationPolicy(raw)
	if err != nil {
		return err
	}
	task.DocumentationPolicy = policy
	return nil
}

func (s *Store) hydrateDocumentationPolicies(ctx context.Context, tasks []core.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	rows, err := s.pool.Query(ctx, `SELECT task_id, payload_json FROM task_documentation_policies WHERE workspace_id=$1 AND task_id=ANY($2::text[])`, workspace(ctx), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[string]*core.DocumentationPolicy{}
	for rows.Next() {
		var taskID string
		var raw []byte
		if err = rows.Scan(&taskID, &raw); err != nil {
			return err
		}
		policy, decodeErr := decodeDocumentationPolicy(raw)
		if decodeErr != nil {
			return decodeErr
		}
		byID[taskID] = policy
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		if policy, ok := byID[tasks[i].ID]; ok {
			tasks[i].DocumentationPolicy = policy
		}
	}
	return nil
}

func (s *Store) documentationPolicyPayload(ctx context.Context, taskID string) ([]byte, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT payload_json FROM task_documentation_policies WHERE workspace_id=$1 AND task_id=$2`, workspace(ctx), taskID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func decodeDocumentationPolicy(raw []byte) (*core.DocumentationPolicy, error) {
	var policy core.DocumentationPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}
