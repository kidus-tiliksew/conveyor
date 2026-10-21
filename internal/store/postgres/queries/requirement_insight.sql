-- feature-requirement-insights RI-5. Mirrored by hand in
-- db/requirement_insight.go: sqlc cannot parse migration 035's template.

-- name: GetRequirementInsightHeadline :one
SELECT workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at
FROM requirement_insight_headlines
WHERE workspace_id = $1 AND requirement_id = $2;

-- name: LockRequirementInsightHeadline :one
SELECT workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at
FROM requirement_insight_headlines
WHERE workspace_id = $1 AND requirement_id = $2
FOR UPDATE;

-- name: UpsertRequirementInsightHeadline :one
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
RETURNING workspace_id, requirement_id, fingerprint, headline, model, tokens_in, tokens_out, generated_by, generated_at;
