// Hand-maintained sqlc-style bindings; see component-persistence and AGENTS.md.
// versions:
//   sqlc v1.28.0
// source: crate_briefing.sql

package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// CrateBriefing is one crate_briefings row (migration 175; feature-crate-insights
// CI-2). LookFirst is the JSON array of entries.
type CrateBriefing struct {
	WorkspaceID string             `json:"workspace_id"`
	CrateID     string             `json:"crate_id"`
	Fingerprint string             `json:"fingerprint"`
	Headline    string             `json:"headline"`
	Account     string             `json:"account"`
	LookFirst   []byte             `json:"look_first"`
	Model       string             `json:"model"`
	TokensIn    int64              `json:"tokens_in"`
	TokensOut   int64              `json:"tokens_out"`
	GeneratedBy string             `json:"generated_by"`
	GeneratedAt pgtype.Timestamptz `json:"generated_at"`
}

const getCrateBriefing = `-- name: GetCrateBriefing :one
SELECT workspace_id, crate_id, fingerprint, headline, account, look_first, model, tokens_in, tokens_out, generated_by, generated_at
FROM crate_briefings
WHERE workspace_id = $1 AND crate_id = $2
`

type GetCrateBriefingParams struct {
	WorkspaceID string `json:"workspace_id"`
	CrateID     string `json:"crate_id"`
}

func (q *Queries) GetCrateBriefing(ctx context.Context, arg GetCrateBriefingParams) (CrateBriefing, error) {
	row := q.db.QueryRow(ctx, getCrateBriefing, arg.WorkspaceID, arg.CrateID)
	var i CrateBriefing
	err := row.Scan(
		&i.WorkspaceID,
		&i.CrateID,
		&i.Fingerprint,
		&i.Headline,
		&i.Account,
		&i.LookFirst,
		&i.Model,
		&i.TokensIn,
		&i.TokensOut,
		&i.GeneratedBy,
		&i.GeneratedAt,
	)
	return i, err
}

const upsertCrateBriefing = `-- name: UpsertCrateBriefing :one
INSERT INTO crate_briefings (workspace_id, crate_id, fingerprint, headline, account, look_first, model, tokens_in, tokens_out, generated_by, generated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (workspace_id, crate_id) DO UPDATE SET
  fingerprint = EXCLUDED.fingerprint,
  headline = EXCLUDED.headline,
  account = EXCLUDED.account,
  look_first = EXCLUDED.look_first,
  model = EXCLUDED.model,
  tokens_in = EXCLUDED.tokens_in,
  tokens_out = EXCLUDED.tokens_out,
  generated_by = EXCLUDED.generated_by,
  generated_at = EXCLUDED.generated_at
WHERE crate_briefings.fingerprint <> EXCLUDED.fingerprint
RETURNING workspace_id, crate_id, fingerprint, headline, account, look_first, model, tokens_in, tokens_out, generated_by, generated_at
`

type UpsertCrateBriefingParams struct {
	WorkspaceID string             `json:"workspace_id"`
	CrateID     string             `json:"crate_id"`
	Fingerprint string             `json:"fingerprint"`
	Headline    string             `json:"headline"`
	Account     string             `json:"account"`
	LookFirst   []byte             `json:"look_first"`
	Model       string             `json:"model"`
	TokensIn    int64              `json:"tokens_in"`
	TokensOut   int64              `json:"tokens_out"`
	GeneratedBy string             `json:"generated_by"`
	GeneratedAt pgtype.Timestamptz `json:"generated_at"`
}

// UpsertCrateBriefing returns pgx.ErrNoRows when the stored row already has
// the fingerprint: the predicate leaves it unchanged and returns nothing.
func (q *Queries) UpsertCrateBriefing(ctx context.Context, arg UpsertCrateBriefingParams) (CrateBriefing, error) {
	row := q.db.QueryRow(ctx, upsertCrateBriefing,
		arg.WorkspaceID,
		arg.CrateID,
		arg.Fingerprint,
		arg.Headline,
		arg.Account,
		arg.LookFirst,
		arg.Model,
		arg.TokensIn,
		arg.TokensOut,
		arg.GeneratedBy,
		arg.GeneratedAt,
	)
	var i CrateBriefing
	err := row.Scan(
		&i.WorkspaceID,
		&i.CrateID,
		&i.Fingerprint,
		&i.Headline,
		&i.Account,
		&i.LookFirst,
		&i.Model,
		&i.TokensIn,
		&i.TokensOut,
		&i.GeneratedBy,
		&i.GeneratedAt,
	)
	return i, err
}

const listCrateBriefingSummaries = `-- name: ListCrateBriefingSummaries :many
SELECT b.crate_id, b.headline, b.generated_at
FROM crate_briefings b
JOIN crates c ON c.workspace_id = b.workspace_id AND c.id = b.crate_id
WHERE b.workspace_id = $1 AND ($2::text = '' OR c.state = $2::text)
`

type ListCrateBriefingSummariesParams struct {
	WorkspaceID string `json:"workspace_id"`
	State       string `json:"state"`
}

type ListCrateBriefingSummariesRow struct {
	CrateID     string             `json:"crate_id"`
	Headline    string             `json:"headline"`
	GeneratedAt pgtype.Timestamptz `json:"generated_at"`
}

// ListCrateBriefingSummaries reads the headline and generation time of every
// stored briefing of the workspace's crates in the listed state, or of every
// state when State is empty, in one statement whatever the number of crates
// (component-crates Public surfaces, GET /v1/crates). The join matches on
// workspace and crate ID, so a crate of another workspace contributes no row.
func (q *Queries) ListCrateBriefingSummaries(ctx context.Context, arg ListCrateBriefingSummariesParams) ([]ListCrateBriefingSummariesRow, error) {
	rows, err := q.db.Query(ctx, listCrateBriefingSummaries, arg.WorkspaceID, arg.State)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ListCrateBriefingSummariesRow
	for rows.Next() {
		var i ListCrateBriefingSummariesRow
		if err := rows.Scan(&i.CrateID, &i.Headline, &i.GeneratedAt); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
