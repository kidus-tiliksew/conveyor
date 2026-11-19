-- feature-crate-insights CI-2, component-crates invariant 29 and Storage,
-- req-crate-insights AC-5.2, AC-5.4, and AC-5.6, and DEC-142(2). At most one
-- generated briefing per crate. The briefing is informative and never
-- authority: writing it appends no event and changes no task, work order,
-- gate, document, or crate field. look_first holds the validated entries as a
-- JSON array. The token counts are stored and never served. No backfill.
CREATE TABLE IF NOT EXISTS crate_briefings (
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 crate_id text NOT NULL,
 fingerprint text NOT NULL,
 headline text NOT NULL,
 account text NOT NULL,
 look_first jsonb NOT NULL CHECK (jsonb_typeof(look_first) = 'array'),
 model text NOT NULL,
 tokens_in bigint NOT NULL CHECK (tokens_in >= 0),
 tokens_out bigint NOT NULL CHECK (tokens_out >= 0),
 generated_by text NOT NULL,
 generated_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, crate_id)
);
