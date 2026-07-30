-- Migration 046: living requirement documents, durable planning sessions, and
-- the lossless retirement of the curated feature tree
-- (spec §§4.2, 9, 13.1, 16; §21.46 change 2).
--
-- Requirements are versioned and confirmed, never gated: a version is proposed
-- by chat or by the monitor's requirements_amended outcome and only an
-- operator confirmation makes it current. requirements.current_version stays
-- NULL until that first confirmation, so a seeded document is visibly pending
-- rather than silently authoritative.

CREATE TABLE requirements (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id           text NOT NULL,
    slug         text NOT NULL,
    title        text NOT NULL,
    current_version integer,
    -- Monotonic largest REQ-n ever issued by this document. A retired
    -- statement's identifier is never reassigned, so a REQ-n citation in code
    -- outlives every blueprint that served it (spec §4.2 items 1 and 4).
    statement_high_water_mark integer NOT NULL DEFAULT 0
        CHECK (statement_high_water_mark >= 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, slug)
);

CREATE TABLE requirement_versions (
    workspace_id    text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    requirement_id  text NOT NULL,
    version         integer NOT NULL CHECK (version > 0),
    content         text NOT NULL,
    statements_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    origin          text NOT NULL
        CHECK (origin IN ('chat', 'drift_amendment', 'feature_migration')),
    origin_session_id text NOT NULL DEFAULT '',
    origin_drift_id   text NOT NULL DEFAULT '',
    confirmed       boolean NOT NULL DEFAULT false,
    confirmed_by    text NOT NULL DEFAULT '',
    confirmed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, requirement_id, version),
    FOREIGN KEY (workspace_id, requirement_id)
        REFERENCES requirements(workspace_id, id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(statements_json) = 'array'),
    -- Confirmation is an audited operator act, so its identity and timestamp
    -- are recorded together or not at all.
    CHECK (
        (confirmed AND confirmed_at IS NOT NULL AND confirmed_by <> '')
        OR
        (NOT confirmed AND confirmed_at IS NULL AND confirmed_by = '')
    )
);

CREATE INDEX requirement_versions_pending_idx
    ON requirement_versions (workspace_id, requirement_id, version DESC)
    WHERE NOT confirmed;

-- The confirmed pointer must name a real version of the same document.
ALTER TABLE requirements
    ADD CONSTRAINT requirements_current_version_fk
        FOREIGN KEY (workspace_id, id, current_version)
        REFERENCES requirement_versions (workspace_id, requirement_id, version);

CREATE TABLE planning_sessions (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id           text NOT NULL,
    title        text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'finalized', 'abandoned')),
    -- Set when the session was opened from a requirement ("Plan work"), which
    -- is what lets a finalized blueprint propose its serves link (§4.2 item 1).
    requirement_context_id  text,
    produced_requirement_id text,
    produced_task_id        text,
    transcript_artifact_id  text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    finalized_at timestamptz,
    PRIMARY KEY (workspace_id, id),
    FOREIGN KEY (workspace_id, requirement_context_id)
        REFERENCES requirements(workspace_id, id) ON DELETE SET NULL,
    FOREIGN KEY (workspace_id, produced_requirement_id)
        REFERENCES requirements(workspace_id, id) ON DELETE SET NULL,
    FOREIGN KEY (workspace_id, produced_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE SET NULL,
    FOREIGN KEY (workspace_id, transcript_artifact_id)
        REFERENCES artifacts(workspace_id, id) ON DELETE SET NULL,
    CHECK ((status = 'finalized') = (finalized_at IS NOT NULL)),
    -- One session finalizes into one artifact type, never both.
    CHECK (NOT (produced_requirement_id IS NOT NULL AND produced_task_id IS NOT NULL))
);

CREATE INDEX planning_sessions_workspace_idx
    ON planning_sessions (workspace_id, updated_at DESC, id);

CREATE TABLE planning_messages (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    session_id   text NOT NULL,
    seq          integer NOT NULL CHECK (seq > 0),
    role         text NOT NULL
        CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    content      text NOT NULL DEFAULT '',
    -- AI SDK UI-message parts verbatim, so a restored session renders exactly
    -- what streamed — including tool activity — without re-deriving it.
    parts_json   jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, session_id, seq),
    FOREIGN KEY (workspace_id, session_id)
        REFERENCES planning_sessions(workspace_id, id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(parts_json) = 'array')
);

-- Requirement and planning mutations are audited like any other event, but
-- they are not task-scoped, so they join the workspace-scoped allowlist.
ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind IN (
        'config.updated', 'workspace.created', 'worker.pairing_issued',
        'worker.enrolled', 'worker.revoked', 'worker.heartbeat',
        'requirement.created', 'requirement.version_proposed',
        'requirement.version_confirmed', 'planning_session.created',
        'planning_session.message_appended', 'planning_session.finalized',
        'planning_session.abandoned', 'feature.migrated'
    ) AND job_id IS NULL)
);

-- Artifact attachment re-homes from features to requirements (§21.46 change 5).
ALTER TABLE artifact_links
    ADD COLUMN requirement_id text;
ALTER TABLE artifact_links
    ADD CONSTRAINT artifact_links_requirement_fk
        FOREIGN KEY (workspace_id, requirement_id)
        REFERENCES requirements(workspace_id, id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- Feature-tree retirement. Order matters: seed documents, preserve every
-- reference as durable lineage, and only then drop the nodes that carried
-- nothing. Nothing here deletes a node that still holds content, a task
-- assignment, or an attachment.
-- ---------------------------------------------------------------------------

-- A node is content-bearing when it holds text, owns a task assignment
-- (directly or through a materialized blueprint child), or holds an artifact.
-- Anything else is empty taxonomy and drops.
CREATE TEMPORARY TABLE migration_046_content_features AS
SELECT feature.id, feature.workspace_id, feature.name, feature.description
FROM features feature
WHERE btrim(coalesce(feature.description, '')) <> ''
   OR EXISTS (
       SELECT 1 FROM tasks task
       WHERE task.feature_id = feature.id
         AND task.workspace_id = feature.workspace_id
   )
   OR EXISTS (
       SELECT 1 FROM tasks child
       JOIN tasks parent
         ON parent.workspace_id = child.workspace_id
        AND parent.id = child.parent_task_id
       WHERE child.feature_id IS NULL
         AND parent.feature_id = feature.id
         AND parent.workspace_id = feature.workspace_id
   )
   OR EXISTS (
       SELECT 1 FROM artifact_links link
       WHERE link.feature_id = feature.id
         AND link.workspace_id = feature.workspace_id
   );

-- Slugs are workspace-unique, but feature names were only unique per parent,
-- so equal names in different branches are disambiguated deterministically by
-- creation order rather than by silently colliding.
CREATE TEMPORARY TABLE migration_046_seeded_requirements AS
SELECT
    source.id AS feature_id,
    source.workspace_id,
    'req-' || source.id AS requirement_id,
    source.name AS title,
    CASE
        WHEN collision.rank = 1 THEN collision.base_slug
        ELSE collision.base_slug || '-' || collision.rank::text
    END AS slug,
    source.description
FROM migration_046_content_features source
JOIN (
    SELECT
        candidate.id,
        candidate.base_slug,
        row_number() OVER (
            PARTITION BY candidate.workspace_id, candidate.base_slug
            ORDER BY candidate.created_at, candidate.id
        ) AS rank
    FROM (
        SELECT
            content.id,
            content.workspace_id,
            feature.created_at,
            -- Mirrors core.RequirementSlug, including its 80-character bound,
            -- so migrated and chat-created slugs are derived identically.
            coalesce(
                nullif(
                    btrim(
                        left(
                            btrim(
                                regexp_replace(lower(content.name), '[^a-z0-9]+', '-', 'g'),
                                '-'
                            ),
                            80
                        ),
                        '-'
                    ),
                    ''
                ),
                'requirement'
            ) AS base_slug
        FROM migration_046_content_features content
        JOIN features feature
          ON feature.id = content.id
         AND feature.workspace_id = content.workspace_id
    ) candidate
) collision ON collision.id = source.id;

INSERT INTO requirements (workspace_id, id, slug, title, current_version, created_at, updated_at)
SELECT seed.workspace_id, seed.requirement_id, seed.slug, seed.title, NULL, feature.created_at, now()
FROM migration_046_seeded_requirements seed
JOIN features feature
  ON feature.id = seed.feature_id
 AND feature.workspace_id = seed.workspace_id;

-- The seed's first version carries the node's accumulated text verbatim and
-- stays pending. Conveyor does not invent REQ-n statements from a description:
-- an empty statement block is honest, and confirmation is where a real block
-- becomes mandatory (core.ConfirmableRequirementVersion).
INSERT INTO requirement_versions (
    workspace_id, requirement_id, version, content, statements_json,
    origin, confirmed, created_at
)
SELECT
    seed.workspace_id,
    seed.requirement_id,
    1,
    seed.title || CASE
        WHEN btrim(coalesce(seed.description, '')) = '' THEN ''
        ELSE E'\n\n' || seed.description
    END,
    '[]'::jsonb,
    'feature_migration',
    false,
    now()
FROM migration_046_seeded_requirements seed;

-- A seed issues no statements, so its high-water mark stays at the column
-- default of zero and the operator's first confirmed block starts at REQ-1.

-- tasks.feature_id assignments convert to durable history links before the
-- column retires (§16). Provenance is the legacy escape hatch: migration 045
-- established that historical edges without a single identifying event carry
-- explicit legacy provenance instead of a guessed identity.
INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind, legacy_created_by_event
)
SELECT DISTINCT seed.workspace_id, 'requirement', seed.requirement_id, 'task', task.id,
       'historical_feature_assignment', 'feature.migrated'
FROM tasks task
JOIN migration_046_seeded_requirements seed
  ON seed.feature_id = task.feature_id
 AND seed.workspace_id = task.workspace_id
ON CONFLICT DO NOTHING;

-- Materialized blueprint children inherit their parent's assignment at
-- creation, but a parent assigned after materialization leaves the child's
-- column NULL. Those inherited assignments are lineage too (AC-5).
INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind, legacy_created_by_event
)
SELECT DISTINCT seed.workspace_id, 'requirement', seed.requirement_id, 'task', child.id,
       'historical_feature_assignment', 'feature.migrated'
FROM tasks child
JOIN tasks parent
  ON parent.workspace_id = child.workspace_id
 AND parent.id = child.parent_task_id
JOIN migration_046_seeded_requirements seed
  ON seed.feature_id = parent.feature_id
 AND seed.workspace_id = parent.workspace_id
WHERE child.feature_id IS NULL
ON CONFLICT DO NOTHING;

-- Feature-scoped artifact attachments re-home onto the seeded requirement.
UPDATE artifact_links link
SET requirement_id = seed.requirement_id,
    feature_id = NULL
FROM migration_046_seeded_requirements seed
WHERE link.feature_id = seed.feature_id
  AND link.workspace_id = seed.workspace_id;

-- Empty nodes drop only now that every surviving reference is recorded
-- elsewhere. A node reaching here holds no text, no assignment, and no
-- attachment, so the delete cannot strand lineage; children are already
-- flattened into independent documents because the corpus has no hierarchy.
DELETE FROM features feature
WHERE NOT EXISTS (
    SELECT 1 FROM migration_046_content_features content
    WHERE content.id = feature.id
      AND content.workspace_id = feature.workspace_id
);

CREATE UNIQUE INDEX artifact_links_requirement_unique
    ON artifact_links (workspace_id, artifact_id, requirement_id, role)
    WHERE requirement_id IS NOT NULL;
CREATE INDEX artifact_links_requirement_idx
    ON artifact_links (requirement_id) WHERE requirement_id IS NOT NULL;

-- The attachment target stays exclusive across the retirement window: the
-- feature column still exists for the surfaces that have not migrated yet,
-- but no link may claim two owners.
ALTER TABLE artifact_links DROP CONSTRAINT artifact_links_check;
ALTER TABLE artifact_links ADD CONSTRAINT artifact_links_check CHECK (
    (task_id IS NOT NULL)::int
    + (feature_id IS NOT NULL)::int
    + (requirement_id IS NOT NULL)::int <= 1
);

-- Preserves the role column migration 027 added to the unattached-artifact
-- uniqueness rule; only the requirement predicate is new.
DROP INDEX artifact_links_workspace_unique;
CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND feature_id IS NULL AND requirement_id IS NULL;

DROP TABLE migration_046_seeded_requirements;
DROP TABLE migration_046_content_features;
