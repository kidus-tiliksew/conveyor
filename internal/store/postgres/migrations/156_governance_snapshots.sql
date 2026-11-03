-- Content-addressed governance snapshots. work_orders keep a digest
-- reference; readers join the body. Identical claim-time JSON is stored once
-- (component-persistence, Schema domains). Go backfills existing inline
-- bodies with EncodeGovernanceSnapshot after this file, then drops the
-- duplicated column.
CREATE TABLE IF NOT EXISTS governance_snapshots (
    workspace_id text NOT NULL,
    digest       text NOT NULL,
    content      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, digest),
    CONSTRAINT governance_snapshots_digest_format_check
        CHECK (digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT governance_snapshots_content_object_check
        CHECK (jsonb_typeof(content) = 'object')
);

ALTER TABLE work_orders
    ADD COLUMN IF NOT EXISTS governance_snapshot_digest text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'work_orders_governance_snapshot_digest_fkey'
    ) THEN
        ALTER TABLE work_orders
            ADD CONSTRAINT work_orders_governance_snapshot_digest_fkey
            FOREIGN KEY (workspace_id, governance_snapshot_digest)
            REFERENCES governance_snapshots (workspace_id, digest);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'work_orders_governance_snapshot_digest_format_check'
    ) THEN
        ALTER TABLE work_orders
            ADD CONSTRAINT work_orders_governance_snapshot_digest_format_check
            CHECK (governance_snapshot_digest IS NULL OR governance_snapshot_digest ~ '^[0-9a-f]{64}$');
    END IF;
END $$;
