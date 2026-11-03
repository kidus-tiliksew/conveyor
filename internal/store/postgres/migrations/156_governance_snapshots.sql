-- Content-addressed governance snapshots. work_orders keep a digest
-- reference; readers join the body. Identical claim-time JSON is stored once
-- (component-persistence, Schema domains).
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

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'work_orders'
          AND column_name = 'governance_snapshot'
    ) THEN
        INSERT INTO governance_snapshots (workspace_id, digest, content)
        SELECT workspace_id,
               encode(sha256(convert_to(governance_snapshot::text, 'UTF8')), 'hex'),
               governance_snapshot
        FROM work_orders
        WHERE governance_snapshot IS NOT NULL
        ON CONFLICT (workspace_id, digest) DO NOTHING;

        ALTER TABLE work_orders
            ADD COLUMN IF NOT EXISTS governance_snapshot_digest text;

        UPDATE work_orders
        SET governance_snapshot_digest = encode(sha256(convert_to(governance_snapshot::text, 'UTF8')), 'hex')
        WHERE governance_snapshot IS NOT NULL
          AND governance_snapshot_digest IS NULL;

        ALTER TABLE work_orders
            DROP CONSTRAINT IF EXISTS work_orders_governance_snapshot_object_check;
        ALTER TABLE work_orders
            DROP COLUMN governance_snapshot;
    END IF;
END $$;

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
