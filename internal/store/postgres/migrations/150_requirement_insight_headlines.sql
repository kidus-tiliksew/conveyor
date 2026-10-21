-- feature-requirement-insights RI-5, DEC-87 (2), req-requirement-insights
-- AC-2.4, AC-2.6, and AC-2.7. At most one generated headline per requirement.
-- The headline is informative and never authority: writing it appends no
-- event and changes no task, work order, gate, or document. No backfill.
CREATE TABLE IF NOT EXISTS requirement_insight_headlines (
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 requirement_id text NOT NULL,
 fingerprint text NOT NULL,
 headline text NOT NULL,
 model text NOT NULL,
 tokens_in bigint NOT NULL CHECK (tokens_in >= 0),
 tokens_out bigint NOT NULL CHECK (tokens_out >= 0),
 generated_by text NOT NULL,
 generated_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, requirement_id)
);
