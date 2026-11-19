-- feature-crate-insights CI-2. Mirrored by hand in db/crate_briefing.go:
-- sqlc cannot parse migration 035's template.

-- name: GetCrateBriefing :one
SELECT workspace_id, crate_id, fingerprint, headline, account, look_first, model, tokens_in, tokens_out, generated_by, generated_at
FROM crate_briefings
WHERE workspace_id = $1 AND crate_id = $2;

-- name: UpsertCrateBriefing :one
-- The fingerprint predicate makes the upsert one atomic compare-and-write:
-- a stored row with an equal fingerprint is left unchanged and no row is
-- returned, so the caller reports the briefing current.
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
RETURNING workspace_id, crate_id, fingerprint, headline, account, look_first, model, tokens_in, tokens_out, generated_by, generated_at;
