// Hand-maintained sqlc-style bindings; see component-persistence and CLAUDE.md.
// versions:
//   sqlc v1.28.0
// source: requirement_insight.sql

package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// RequirementInsightHeadline is one requirement_insight_headlines row
// (migration 150; feature-requirement-insights RI-5).
type RequirementInsightHeadline struct {
	WorkspaceID   string             `json:"workspace_id"`
	RequirementID string             `json:"requirement_id"`
	Fingerprint   string             `json:"fingerprint"`
	Headline      string             `json:"headline"`
	Model         string             `json:"model"`
	TokensIn      int64              `json:"tokens_in"`
	TokensOut     int64              `json:"tokens_out"`
	GeneratedBy   string             `json:"generated_by"`
	GeneratedAt   pgtype.Timestamptz `json:"generated_at"`
}

const getRequirementInsightHeadline = `-- name: GetRequirementInsightHeadline :one
SELECT workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at
FROM requirement_insight_headlines
WHERE workspace_id = $1 AND requirement_id = $2
`

type GetRequirementInsightHeadlineParams struct {
	WorkspaceID   string `json:"workspace_id"`
	RequirementID string `json:"requirement_id"`
}

func (q *Queries) GetRequirementInsightHeadline(ctx context.Context, arg GetRequirementInsightHeadlineParams) (RequirementInsightHeadline, error) {
	row := q.db.QueryRow(ctx, getRequirementInsightHeadline, arg.WorkspaceID, arg.RequirementID)
	var i RequirementInsightHeadline
	err := row.Scan(
		&i.WorkspaceID,
		&i.RequirementID,
		&i.Fingerprint,
		&i.Headline,
		&i.Model,
		&i.TokensIn,
		&i.TokensOut,
		&i.GeneratedBy,
		&i.GeneratedAt,
	)
	return i, err
}

const lockRequirementInsightHeadline = `-- name: LockRequirementInsightHeadline :one
SELECT workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at
FROM requirement_insight_headlines
WHERE workspace_id = $1 AND requirement_id = $2
FOR UPDATE
`

type LockRequirementInsightHeadlineParams struct {
	WorkspaceID   string `json:"workspace_id"`
	RequirementID string `json:"requirement_id"`
}

func (q *Queries) LockRequirementInsightHeadline(ctx context.Context, arg LockRequirementInsightHeadlineParams) (RequirementInsightHeadline, error) {
	row := q.db.QueryRow(ctx, lockRequirementInsightHeadline, arg.WorkspaceID, arg.RequirementID)
	var i RequirementInsightHeadline
	err := row.Scan(
		&i.WorkspaceID,
		&i.RequirementID,
		&i.Fingerprint,
		&i.Headline,
		&i.Model,
		&i.TokensIn,
		&i.TokensOut,
		&i.GeneratedBy,
		&i.GeneratedAt,
	)
	return i, err
}

const upsertRequirementInsightHeadline = `-- name: UpsertRequirementInsightHeadline :one
INSERT INTO requirement_insight_headlines (workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (workspace_id, requirement_id) DO UPDATE SET
  fingerprint = EXCLUDED.fingerprint,
  headline = EXCLUDED.headline,
  model = EXCLUDED.model,
  tokens_in = EXCLUDED.tokens_in,
  tokens_out = EXCLUDED.tokens_out,
  generated_by = EXCLUDED.generated_by,
  generated_at = EXCLUDED.generated_at
RETURNING workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at
`

type UpsertRequirementInsightHeadlineParams struct {
	WorkspaceID   string             `json:"workspace_id"`
	RequirementID string             `json:"requirement_id"`
	Fingerprint   string             `json:"fingerprint"`
	Headline      string             `json:"headline"`
	Model         string             `json:"model"`
	TokensIn      int64              `json:"tokens_in"`
	TokensOut     int64              `json:"tokens_out"`
	GeneratedBy   string             `json:"generated_by"`
	GeneratedAt   pgtype.Timestamptz `json:"generated_at"`
}

func (q *Queries) UpsertRequirementInsightHeadline(ctx context.Context, arg UpsertRequirementInsightHeadlineParams) (RequirementInsightHeadline, error) {
	row := q.db.QueryRow(ctx, upsertRequirementInsightHeadline,
		arg.WorkspaceID,
		arg.RequirementID,
		arg.Fingerprint,
		arg.Headline,
		arg.Model,
		arg.TokensIn,
		arg.TokensOut,
		arg.GeneratedBy,
		arg.GeneratedAt,
	)
	var i RequirementInsightHeadline
	err := row.Scan(
		&i.WorkspaceID,
		&i.RequirementID,
		&i.Fingerprint,
		&i.Headline,
		&i.Model,
		&i.TokensIn,
		&i.TokensOut,
		&i.GeneratedBy,
		&i.GeneratedAt,
	)
	return i, err
}
